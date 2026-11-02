// Package imagescmd defines `ocictl images verify`.
package imagescmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/helmctl"
	"github.com/truvity/ocictl/pkg/imagecheck"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// RegistryOptions maps --profile to the registry options of the checks. Tests
// replace it to talk plain HTTP to a fake registry; nothing else should.
var RegistryOptions = func(profile string) ocipush.Options {
	return ocipush.Options{AWSProfile: profile}
}

// Command returns `images`, whose only subcommand is verify.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "images",
		Usage: "Checks on the images a release names",
		Commands: []*cli.Command{
			{
				Name: "verify",
				Usage: "Check that every images.* entry of a release manifest resolves in its registry and offers the " +
					"named platforms (registry read access only)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "manifest", Usage: "Release manifest (see chart goreleaser-manifest)", Required: true},
					&cli.StringSliceFlag{
						Name:  "platforms",
						Usage: "os/arch[/variant] every image must offer in an index, e.g. linux/amd64,linux/arm64 (default: none, the image only has to resolve)",
					},
					&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
				},
				Action: runVerify,
			},
		},
	}
}

func runVerify(ctx context.Context, cmd *cli.Command) error {
	return verify(ctx, os.Stdout, cmd)
}

func verify(ctx context.Context, w io.Writer, cmd *cli.Command) error {
	platforms, err := imagecheck.ParsePlatforms(cmd.StringSlice("platforms"))
	if err != nil {
		return err
	}

	manifest, err := helmctl.LoadManifest(cmd.String("manifest"))
	if err != nil {
		return err
	}

	images, err := imagecheck.FromValues(manifest.Values)
	if err != nil {
		return err
	}

	// A guard must refuse an empty sweep: a manifest with no images is more
	// likely a wrong file than a release with nothing to check.
	if len(images) == 0 {
		return errors.New("the manifest has no values.images entries: nothing to verify")
	}

	results, err := imagecheck.Verify(ctx, images, platforms, RegistryOptions(cmd.String("profile")))
	for _, r := range results {
		_, _ = fmt.Fprintln(w, r)
	}

	return err
}
