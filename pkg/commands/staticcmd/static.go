// Package staticcmd defines `ocictl static push` and `ocictl digest`.
package staticcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/commands/annot"
	"github.com/truvity/ocictl/pkg/commands/pushout"
	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/static"
)

// RegistryOptions maps --profile to the registry options of a push. Tests
// replace it to talk plain HTTP to a fake registry; nothing else should.
var RegistryOptions = func(profile string) ocipush.Options {
	return ocipush.Options{AWSProfile: profile}
}

func shapeFlags() []cli.Flag {
	return append([]cli.Flag{
		&cli.StringFlag{Name: "shape", Usage: "Manifest shape: artifact (empty config, artifactType) or image (image config)", Value: string(static.ShapeArtifact)},
		&cli.StringFlag{Name: "architecture", Usage: "Image config architecture (shape image)", Value: static.DefaultArchitecture},
		&cli.StringFlag{Name: "os", Usage: "Image config os (shape image)", Value: static.DefaultOS},
		&cli.StringFlag{Name: "created", Usage: ocipush.CreatedFlagUsage},
	}, annot.Flags()...)
}

// Command returns `static`, whose only subcommand is push.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "static",
		Usage: "Static sites as OCI artifacts",
		Commands: []*cli.Command{
			{
				Name:  "push",
				Usage: "Pack a directory into a deterministic OCI artifact and push it under a version tag (idempotent)",
				Flags: append([]cli.Flag{
					&cli.StringFlag{Name: "dir", Usage: "Directory to publish (every file, at its path relative to this root)", Required: true},
					&cli.StringFlag{Name: "repository", Usage: "host/path with no tag", Required: true},
					&cli.StringFlag{Name: "tag", Usage: "Version tag (\"+\" becomes \"_\")", Required: true},
					&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
					&cli.BoolFlag{Name: "dry-run", Usage: "Pack and print the digests; push nothing"},
				}, append(shapeFlags(), pushout.Flags()...)...),
				Action: runPush,
			},
		},
	}
}

// DigestCommand returns `digest <dir>`.
func DigestCommand() *cli.Command {
	return &cli.Command{
		Name:      "digest",
		Usage:     "Print the layer and manifest digests of a directory (what `static push --dry-run` prints); pushes nothing",
		ArgsUsage: "<dir>",
		Flags:     append(shapeFlags(), pushout.DigestFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return errors.New("digest takes exactly one argument: the directory")
			}

			if err := pushout.Validate(cmd); err != nil {
				return err
			}

			art, _, err := build(ctx, cmd.Args().First(), "", cmd)
			if err != nil {
				return err
			}

			return printDigests(os.Stdout, cmd, art)
		},
	}
}

func build(ctx context.Context, dir, tag string, cmd *cli.Command) (ocipush.Artifact, int, error) {
	shape, err := static.ParseShape(cmd.String("shape"))
	if err != nil {
		return ocipush.Artifact{}, 0, err
	}

	annotations, err := annot.Resolve(ctx, cmd)
	if err != nil {
		return ocipush.Artifact{}, 0, err
	}

	created, err := ocipush.ResolveCreated(cmd.String("created"))
	if err != nil {
		return ocipush.Artifact{}, 0, err
	}

	packed, err := static.Pack(dir)
	if err != nil {
		return ocipush.Artifact{}, 0, err
	}

	art, err := static.Artifact(packed, static.Options{
		Shape:        shape,
		Tag:          tag,
		Architecture: cmd.String("architecture"),
		OS:           cmd.String("os"),
		Annotations:  annotations,
		Created:      created,
	})

	return art, len(packed.Files), err
}

func printDigests(w io.Writer, cmd *cli.Command, art ocipush.Artifact) error {
	d, err := static.Describe(art)
	if err != nil {
		return err
	}

	return pushout.EmitDigest(w, cmd, pushout.DigestResult{
		Digest:       d.Manifest,
		LayerDigest:  d.Layer,
		ConfigDigest: d.Config,
		ArtifactType: ocipush.EffectiveArtifactType(art),
		MediaType:    ocispec.MediaTypeImageManifest,
	}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "layer %s\nconfig %s\nmanifest %s\n", d.Layer, d.Config, d.Manifest)

		return err
	})
}

func runPush(ctx context.Context, cmd *cli.Command) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := pushout.Validate(cmd); err != nil {
		return err
	}

	art, files, err := build(ctx, cmd.String("dir"), cmd.String("tag"), cmd)
	if err != nil {
		return err
	}

	if cmd.Bool("dry-run") {
		logger.InfoContext(ctx, "dry run: nothing pushed", slog.Int("files", files), slog.Int("layerBytes", len(art.Layer)))

		return printDigests(os.Stdout, cmd, art)
	}

	repository := cmd.String("repository")

	outcome, dgst, err := static.Push(ctx, logger, repository, art, RegistryOptions(cmd.String("profile")))
	if err != nil {
		return err
	}

	verb, status := "pushed", pushout.StatusPushed
	if outcome == static.AlreadyPresent {
		verb, status = "already present", pushout.StatusAlreadyPresent
	}

	return pushout.Emit(os.Stdout, cmd, pushout.New(repository, art, dgst, status), func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s: %s:%s@%s\n", verb, repository, art.Tag, dgst)

		return err
	})
}
