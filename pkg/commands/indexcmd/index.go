// Package indexcmd defines `ocictl index create`.
package indexcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/imageindex"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// RegistryOptions maps --profile to the registry options. Tests replace it to
// talk plain HTTP to a fake registry; nothing else should.
var RegistryOptions = func(profile string) ocipush.Options {
	return ocipush.Options{AWSProfile: profile}
}

// Command returns `index`, whose only subcommand is create.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "index",
		Usage: "OCI image indexes",
		Commands: []*cli.Command{
			{
				Name: "create",
				Usage: "Merge single-platform images, built in separate jobs, into one image index under a tag " +
					"(idempotent, no Docker daemon)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repository", Usage: "host/path of the index, no tag", Required: true},
					&cli.StringFlag{Name: "tag", Usage: "Tag of the index", Required: true},
					&cli.StringSliceFlag{
						Name:     "manifest",
						Usage:    "<host/path>@<digest>[:<os>/<arch>[/<variant>]] of one image (repeatable); the platform is read from the image when omitted",
						Required: true,
					},
					&cli.StringSliceFlag{Name: "annotation", Usage: "Index annotation key=value (repeatable)"},
					&cli.StringFlag{Name: "created", Usage: ocipush.CreatedFlagUsage},
					&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
					&cli.StringFlag{Name: "output", Usage: "Result format on stdout: text or json", Value: "text"},
				},
				Action: runCreate,
			},
		},
	}
}

func runCreate(ctx context.Context, cmd *cli.Command) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	asJSON := false

	switch f := cmd.String("output"); f {
	case "text", "":
	case "json":
		asJSON = true
	default:
		return fmt.Errorf("--output %q: want text or json", f)
	}

	inputs := make([]imageindex.Input, 0, len(cmd.StringSlice("manifest")))

	for _, s := range cmd.StringSlice("manifest") {
		in, err := imageindex.ParseInput(s)
		if err != nil {
			return err
		}

		inputs = append(inputs, in)
	}

	annotations, err := parseAnnotations(cmd.StringSlice("annotation"))
	if err != nil {
		return err
	}

	created, err := ocipush.ResolveCreated(cmd.String("created"))
	if err != nil {
		return err
	}

	res, err := imageindex.Create(ctx, logger, imageindex.Spec{
		Repository:  cmd.String("repository"),
		Tag:         cmd.String("tag"),
		Inputs:      inputs,
		Annotations: annotations,
		Created:     created,
	}, RegistryOptions(cmd.String("profile")))
	if err != nil {
		return err
	}

	status := "pushed"
	if res.AlreadyPresent {
		status = "already-present"
	}

	if asJSON {
		return writeJSON(os.Stdout, map[string]any{
			"ref":          fmt.Sprintf("%s:%s@%s", res.Repository, res.Tag, res.Digest),
			"repository":   res.Repository,
			"tag":          res.Tag,
			"digest":       res.Digest,
			"status":       status,
			"artifactType": "",
			"mediaType":    ocispec.MediaTypeImageIndex,
			"platforms":    res.Platforms,
		})
	}

	verb := "pushed"
	if res.AlreadyPresent {
		verb = "already present"
	}

	_, err = fmt.Fprintf(os.Stdout, "%s: %s:%s@%s\n", verb, res.Repository, res.Tag, res.Digest)

	return err
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "%s\n", data)

	return err
}

func parseAnnotations(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(kvs))

	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, errors.New("--annotation " + fmt.Sprintf("%q", kv) + ": want key=value")
		}

		out[k] = v
	}

	return out, nil
}
