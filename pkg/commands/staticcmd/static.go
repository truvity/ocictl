// Package staticcmd defines `ocictl static push` and `ocictl digest`.
package staticcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/static"
)

// RegistryOptions maps --profile to the registry options of a push. Tests
// replace it to talk plain HTTP to a fake registry; nothing else should.
var RegistryOptions = func(profile string) ocipush.Options {
	return ocipush.Options{AWSProfile: profile}
}

func shapeFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "shape", Usage: "Manifest shape: artifact (empty config, artifactType) or image (image config)", Value: string(static.ShapeArtifact)},
		&cli.StringFlag{Name: "architecture", Usage: "Image config architecture (shape image)", Value: static.DefaultArchitecture},
		&cli.StringFlag{Name: "os", Usage: "Image config os (shape image)", Value: static.DefaultOS},
		&cli.StringSliceFlag{Name: "annotation", Usage: "Manifest annotation key=value (repeatable)"},
		&cli.StringFlag{Name: "created", Usage: ocipush.CreatedFlagUsage},
	}
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
				}, shapeFlags()...),
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
		Flags:     shapeFlags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return errors.New("digest takes exactly one argument: the directory")
			}

			art, _, err := build(cmd.Args().First(), "", cmd)
			if err != nil {
				return err
			}

			return printDigests(os.Stdout, art)
		},
	}
}

func parseAnnotations(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(kvs))

	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--annotation %q: want key=value", kv)
		}

		out[k] = v
	}

	return out, nil
}

func build(dir, tag string, cmd *cli.Command) (ocipush.Artifact, int, error) {
	shape, err := static.ParseShape(cmd.String("shape"))
	if err != nil {
		return ocipush.Artifact{}, 0, err
	}

	annotations, err := parseAnnotations(cmd.StringSlice("annotation"))
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

func printDigests(w io.Writer, art ocipush.Artifact) error {
	d, err := static.Describe(art)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "layer %s\nconfig %s\nmanifest %s\n", d.Layer, d.Config, d.Manifest)

	return err
}

func runPush(ctx context.Context, cmd *cli.Command) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	art, files, err := build(cmd.String("dir"), cmd.String("tag"), cmd)
	if err != nil {
		return err
	}

	if cmd.Bool("dry-run") {
		logger.InfoContext(ctx, "dry run: nothing pushed", slog.Int("files", files), slog.Int("layerBytes", len(art.Layer)))

		return printDigests(os.Stdout, art)
	}

	repository := cmd.String("repository")

	outcome, dgst, err := static.Push(ctx, logger, repository, art, RegistryOptions(cmd.String("profile")))
	if err != nil {
		return err
	}

	verb := "pushed"
	if outcome == static.AlreadyPresent {
		verb = "already present"
	}

	_, _ = fmt.Fprintf(os.Stdout, "%s: %s:%s@%s\n", verb, repository, art.Tag, dgst)

	return nil
}
