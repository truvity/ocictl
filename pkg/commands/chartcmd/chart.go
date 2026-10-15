// Package chartcmd defines the chart commands: the whole of the former
// helmctl. They are mounted as `ocictl chart` and, unchanged, as the
// top-level commands of the deprecated `helmctl` name.
package chartcmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/goreleaserdist"
	"github.com/truvity/ocictl/pkg/helmctl"
)

// Commands returns goreleaser-manifest, package and push.
func Commands(logger *slog.Logger) []*cli.Command {
	return []*cli.Command{
		{
			Name: "goreleaser-manifest",
			Usage: "Convert a GoReleaser dist/ directory into a release manifest " +
				"(version + digest-pinned image values) for `package --manifest`",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:     "goreleaser-dist",
					Usage:    "Path to the GoReleaser dist directory (artifacts.json + metadata.json)",
					Required: true,
				},
				&cli.StringSliceFlag{
					Name:  "images",
					Usage: "Only include these image component keys (default: all images in the dist)",
				},
				&cli.StringFlag{
					Name:    "output",
					Aliases: []string{"o"},
					Usage:   "Manifest output file (\"-\" = stdout)",
					Value:   "-",
				},
			},
			Action: func(_ context.Context, cmd *cli.Command) error {
				dist, err := goreleaserdist.Load(cmd.String("goreleaser-dist"))
				if err != nil {
					return err
				}

				manifest, err := dist.Manifest(goreleaserdist.ManifestOptions{
					Images: cmd.StringSlice("images"),
				})
				if err != nil {
					return err
				}

				return manifest.Write(cmd.String("output"))
			},
		},
		{
			Name:  "package",
			Usage: "Package a chart with version + values injection (missing dependencies are resolved)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "chart", Usage: "Path to chart directory", Required: true},
				&cli.StringFlag{Name: "version", Usage: "Version to inject (or use --manifest)"},
				&cli.StringFlag{Name: "app-version", Usage: "appVersion to inject (default: version)"},
				&cli.StringFlag{
					Name:  "manifest",
					Usage: "Release manifest: version/appVersion + values to bake in (see `goreleaser-manifest`)",
				},
				&cli.BoolFlag{Name: "require-image-digests", Usage: "Fail unless every values.yaml images.* entry has a digest"},
				&cli.StringFlag{Name: "output", Usage: "Output directory for .tgz", Value: "."},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				cfg := helmctl.PackageConfig{
					ChartDir:            cmd.String("chart"),
					Version:             cmd.String("version"),
					AppVersion:          cmd.String("app-version"),
					RequireImageDigests: cmd.Bool("require-image-digests"),
					// Missing dependencies are resolved; present ones package as before.
					VendorDependencies: true,
					OutputDir:          cmd.String("output"),
				}

				if manifestPath := cmd.String("manifest"); manifestPath != "" {
					manifest, err := helmctl.LoadManifest(manifestPath)
					if err != nil {
						return err
					}

					cfg.ValuesOverlay = manifest.Values

					if cfg.Version == "" {
						cfg.Version = manifest.Version
					}

					if cfg.AppVersion == "" {
						cfg.AppVersion = manifest.AppVersion
					}
				}

				if cfg.Version == "" {
					return fmt.Errorf("either --version or --manifest is required")
				}

				result, err := helmctl.Package(ctx, logger, cfg)
				if err != nil {
					return err
				}

				_, _ = fmt.Fprintf(os.Stdout, "%s\n", result.TgzPath)

				return nil
			},
		},
		{
			Name:  "push",
			Usage: "Push a packaged chart .tgz to an OCI registry (deterministic, ORAS-based)",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "tgz", Usage: "Path to .tgz file", Required: true},
				&cli.StringFlag{Name: "registry", Usage: "OCI registry URL", Required: true},
				&cli.StringFlag{Name: "repository", Usage: "Chart path in registry", Required: true},
				&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
				&cli.StringFlag{Name: "name", Usage: "Chart name (for OCI config blob)", Required: true},
				&cli.StringFlag{Name: "version", Usage: "Chart version (for OCI tag + config)", Required: true},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				result, err := helmctl.Push(ctx, logger, helmctl.PushConfig{
					TgzPath:    cmd.String("tgz"),
					Registry:   cmd.String("registry"),
					Repository: cmd.String("repository"),
					AWSProfile: cmd.String("profile"),
					Meta: helmctl.ChartMeta{
						Name:       cmd.String("name"),
						Version:    cmd.String("version"),
						AppVersion: cmd.String("version"),
						APIVersion: "v2",
						Type:       "application",
					},
				})
				if err != nil {
					return err
				}

				_, _ = fmt.Fprintf(os.Stdout, "pushed: %s (digest: %s)\n", result.Ref, result.Digest)

				return nil
			},
		},
	}
}
