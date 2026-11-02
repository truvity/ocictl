// Package imagecmd defines `ocictl image file-digest` and `ocictl image extract`.
package imagecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/imagefile"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// RegistryOptions maps --profile to the registry options. Tests replace it to
// talk plain HTTP to a fake registry; nothing else should.
var RegistryOptions = func(profile string) ocipush.Options {
	return ocipush.Options{AWSProfile: profile}
}

func commonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "path", Usage: "Absolute path of the file in the flattened image, e.g. /usr/libexec/app/entry", Required: true},
		&cli.StringFlag{Name: "platform", Usage: "os/arch[/variant] of the image to read when the digest is an index"},
		&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
	}
}

// Command returns `image`, with file-digest and extract.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "image",
		Usage: "Read files out of a remote image, without a daemon",
		Commands: []*cli.Command{
			{
				Name:      "file-digest",
				Usage:     "Print sha256:<digest> of a file in the flattened image (whiteouts honoured); exit 2 when it is not there",
				ArgsUsage: "<host/path>@<digest>",
				Flags:     append(commonFlags(), &cli.StringFlag{Name: "expect", Usage: "Exit non-zero unless the file's digest is this one (sha256:...)"}),
				Action:    runFileDigest,
			},
			{
				Name:      "extract",
				Usage:     "Write a file of the flattened image to -o (\"-\" is stdout); exit 2 when it is not there",
				ArgsUsage: "<host/path>@<digest>",
				Flags:     append(commonFlags(), &cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "Destination file (\"-\" for stdout)", Required: true}),
				Action:    runExtract,
			},
		},
	}
}

func target(cmd *cli.Command) (imagefile.Image, error) {
	if cmd.NArg() != 1 {
		return imagefile.Image{}, errors.New("takes exactly one argument: <host/path>@<digest>")
	}

	repo, dgst, err := parseRef(cmd.Args().First())
	if err != nil {
		return imagefile.Image{}, err
	}

	return imagefile.Image{Repository: repo, Digest: dgst, Platform: cmd.String("platform")}, nil
}

func runFileDigest(ctx context.Context, cmd *cli.Command) error {
	img, err := target(cmd)
	if err != nil {
		return err
	}

	res, err := imagefile.Open(ctx, img, cmd.String("path"), io.Discard, RegistryOptions(cmd.String("profile")))
	if err != nil {
		return err
	}

	if want := cmd.String("expect"); want != "" && want != res.Digest {
		return fmt.Errorf("%s in %s@%s has digest %s, expected %s", cmd.String("path"), img.Repository, img.Digest, res.Digest, want)
	}

	_, err = fmt.Fprintln(os.Stdout, res.Digest)

	return err
}

func runExtract(ctx context.Context, cmd *cli.Command) error {
	img, err := target(cmd)
	if err != nil {
		return err
	}

	opts := RegistryOptions(cmd.String("profile"))
	dest := cmd.String("output")

	if dest == "-" {
		_, err := imagefile.Open(ctx, img, cmd.String("path"), os.Stdout, opts)

		return err
	}

	// Write beside the destination and rename, so a failure leaves nothing half-written.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".ocictl-extract-*")
	if err != nil {
		return err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck

	res, err := imagefile.Open(ctx, img, cmd.String("path"), tmp, opts)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		return err
	}

	// The tar mode, without the setuid/setgid/sticky bits, never wider than 0755.
	if err := os.Chmod(tmp.Name(), os.FileMode(res.Mode&0o755)); err != nil { //nolint:gosec // masked to rwxr-xr-x
		return err
	}

	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}

	_, err = fmt.Fprintf(os.Stdout, "%s %s\n", res.Digest, dest)

	return err
}

// parseRef splits <host/path>@<digest>; a tag before the @ is dropped.
func parseRef(ref string) (repository, dgst string, err error) {
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
