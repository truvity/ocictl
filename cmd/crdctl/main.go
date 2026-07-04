// Command crdctl fetches upstream CRDs at pinned versions, builds CRD-only
// Helm charts, and publishes them to an OCI registry.
//
// Usage:
//
//	crdctl build --config <crdctl.yaml>
//	crdctl publish --config <crdctl.yaml> --registry <url> --repository <path>
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/crdctl"
)

var Version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	app := &cli.Command{
		Name:    "crdctl",
		Usage:   "Fetch upstream CRDs, repack into Helm charts, and publish to OCI registries",
		Version: Version,
		Commands: []*cli.Command{
			{
				Name:  "build",
				Usage: "Fetch CRDs from GitHub and generate chart templates/ (no push)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Usage: "Path to crdctl.yaml", Required: true},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					result, err := crdctl.Build(ctx, logger, cmd.String("config"))
					if err != nil {
						return err
					}

					_, _ = fmt.Fprintf(os.Stdout, "built: %s v%s (%s)\n", result.Name, result.Version, result.ChartDir)

					return nil
				},
			},
			{
				Name:  "publish",
				Usage: "Fetch CRDs + package + push to OCI registry (full pipeline)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Usage: "Path to crdctl.yaml", Required: true},
					&cli.StringFlag{Name: "registry", Usage: "OCI registry URL", Required: true},
					&cli.StringFlag{Name: "repository", Usage: "Chart path in registry", Required: true},
					&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return crdctl.Publish(ctx, logger, crdctl.PublishConfig{
						ConfigPath: cmd.String("config"),
						Registry:   cmd.String("registry"),
						Repository: cmd.String("repository"),
						AWSProfile: cmd.String("profile"),
					})
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
