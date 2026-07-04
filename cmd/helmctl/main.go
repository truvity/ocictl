// Command helmctl provides deterministic Helm chart packaging and OCI push.
//
// Usage:
//
//	helmctl package --chart <dir> --version <ver> --output <dir>
//	helmctl push --tgz <file> --registry <url> --repository <path> [--profile <aws>] --version <ver> --name <name>
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/helmctl"
)

var Version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	app := &cli.Command{
		Name:    "helmctl",
		Usage:   "Deterministic Helm chart packaging and OCI push",
		Version: Version,
		Commands: []*cli.Command{
			{
				Name:  "package",
				Usage: "Package a chart with version injection (source is never modified)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "chart", Usage: "Path to chart directory", Required: true},
					&cli.StringFlag{Name: "version", Usage: "Version to inject", Required: true},
					&cli.StringFlag{Name: "output", Usage: "Output directory for .tgz", Value: "."},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := helmctl.Package(ctx, logger, helmctl.PackageConfig{
						ChartDir:  cmd.String("chart"),
						Version:   cmd.String("version"),
						OutputDir: cmd.String("output"),
					})
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
		},
	}

	if err := app.Run(ctx, os.Args); err != nil {
		logger.ErrorContext(ctx, "command failed", slog.Any("error", err))
		cancel()
		os.Exit(1)
	}

	cancel()
}
