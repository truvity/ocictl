// Package referrerscmd defines `ocictl attach` and `ocictl referrers`.
package referrerscmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/ocipush"
)

// RegistryOptions maps --profile to the registry options. Tests replace it to
// talk plain HTTP to a fake registry; nothing else should.
var RegistryOptions = func(profile string) ocipush.Options {
	return ocipush.Options{AWSProfile: profile}
}

// AttachCommand returns `attach`.
func AttachCommand() *cli.Command {
	return &cli.Command{
		Name: "attach",
		Usage: "Attach files to an existing manifest as an OCI referrer (subject), on a registry with or without the " +
			"referrers API",
		ArgsUsage: "<file>...",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "subject", Usage: "The manifest to attach to: <host/path>@<digest> (a digest, never a tag)", Required: true},
			&cli.StringFlag{Name: "artifact-type", Usage: "Artifact type of the referrer, e.g. application/spdx+json", Required: true},
			&cli.StringFlag{Name: "media-type", Usage: "Media type of every file layer", Value: "application/octet-stream"},
			&cli.StringSliceFlag{Name: "annotation", Usage: "Manifest annotation key=value (repeatable)"},
			&cli.StringFlag{Name: "created", Usage: ocipush.CreatedFlagUsage},
			&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
			&cli.StringFlag{Name: "output", Usage: "Result format on stdout: text or json", Value: "text"},
		},
		Action: runAttach,
	}
}

// ReferrersCommand returns `referrers`.
func ReferrersCommand() *cli.Command {
	return &cli.Command{
		Name:      "referrers",
		Usage:     "List the manifests that refer to a manifest (referrers API, or the sha256-<digest> tag schema)",
		ArgsUsage: "<host/path>@<digest>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "artifact-type", Usage: "Only referrers of this artifact type"},
			&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
			&cli.StringFlag{Name: "output", Usage: "Result format on stdout: text or json", Value: "text"},
		},
		Action: runReferrers,
	}
}

// ParseSubject splits <host/path>@<digest>. A tag before the @ is dropped: the
// digest alone names the subject.
func ParseSubject(ref string) (repository, dgst string, err error) {
	repository, dgst, ok := strings.Cut(ref, "@")
	if !ok || repository == "" {
		return "", "", fmt.Errorf("%q: want <host/path>@<digest>", ref)
	}

	if err := digest.Digest(dgst).Validate(); err != nil {
		return "", "", fmt.Errorf("%q: %w", ref, err)
	}

	if i := strings.LastIndex(repository, "/"); i >= 0 {
		if c := strings.Index(repository[i+1:], ":"); c >= 0 {
			repository = repository[:i+1+c]
		}
	}

	return repository, dgst, nil
}

func checkOutput(cmd *cli.Command) (asJSON bool, err error) {
	switch f := cmd.String("output"); f {
	case "text", "":
		return false, nil
	case "json":
		return true, nil
	default:
		return false, fmt.Errorf("--output %q: want text or json", f)
	}
}

func runAttach(ctx context.Context, cmd *cli.Command) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	asJSON, err := checkOutput(cmd)
	if err != nil {
		return err
	}

	if cmd.NArg() == 0 {
		return errors.New("attach takes at least one file")
	}

	repository, subject, err := ParseSubject(cmd.String("subject"))
	if err != nil {
		return fmt.Errorf("--subject %w", err)
	}

	annotations, err := parseAnnotations(cmd.StringSlice("annotation"))
	if err != nil {
		return err
	}

	created, err := ocipush.ResolveCreated(cmd.String("created"))
	if err != nil {
		return err
	}

	blobs := make([]ocipush.Blob, 0, cmd.NArg())

	for _, path := range cmd.Args().Slice() {
		data, err := os.ReadFile(path) //nolint:gosec // the caller names the files to attach
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		blobs = append(blobs, ocipush.Blob{
			MediaType:   cmd.String("media-type"),
			Data:        data,
			Annotations: map[string]string{ocispec.AnnotationTitle: filepath.Base(path)},
		})
	}

	opts := RegistryOptions(cmd.String("profile"))

	res, err := ocipush.Attach(ctx, logger, repository, subject, ocipush.Artifact{
		ArtifactType: cmd.String("artifact-type"),
		Annotations:  annotations,
		Created:      created,
		Blobs:        blobs,
	}, opts)
	if err != nil {
		return err
	}

	if asJSON {
		return writeJSON(os.Stdout, map[string]any{
			"ref":          repository + "@" + res.Digest,
			"repository":   repository,
			"digest":       res.Digest,
			"subject":      res.Subject.Digest.String(),
			"artifactType": cmd.String("artifact-type"),
			"mode":         res.Mode,
		})
	}

	_, err = fmt.Fprintf(os.Stdout, "attached: %s@%s (subject %s, %s)\n", repository, res.Digest, res.Subject.Digest, res.Mode)

	return err
}

func runReferrers(ctx context.Context, cmd *cli.Command) error {
	asJSON, err := checkOutput(cmd)
	if err != nil {
		return err
	}

	if cmd.NArg() != 1 {
		return errors.New("referrers takes exactly one argument: <host/path>@<digest>")
	}

	repository, subject, err := ParseSubject(cmd.Args().First())
	if err != nil {
		return err
	}

	list, mode, err := ocipush.Referrers(ctx, repository, subject, cmd.String("artifact-type"), RegistryOptions(cmd.String("profile")))
	if err != nil {
		return err
	}

	if asJSON {
		if list == nil {
			list = []ocispec.Descriptor{}
		}

		return writeJSON(os.Stdout, map[string]any{"subject": subject, "mode": mode, "referrers": list})
	}

	for _, d := range list {
		if _, err := fmt.Fprintf(os.Stdout, "%s %s %d\n", d.Digest, d.ArtifactType, d.Size); err != nil {
			return err
		}
	}

	return nil
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
			return nil, fmt.Errorf("--annotation %q: want key=value", kv)
		}

		out[k] = v
	}

	return out, nil
}
