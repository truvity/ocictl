// Package crdcmd defines the crd commands: the whole of the former crdctl.
// They are mounted as `ocictl crd` and, unchanged, as the top-level commands
// of the deprecated `crdctl` name.
package crdcmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/crdctl"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// Commands returns build and publish.
func Commands(logger *slog.Logger) []*cli.Command {
	return []*cli.Command{
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
				&cli.StringFlag{Name: "created", Usage: ocipush.CreatedFlagUsage},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				created, err := ocipush.ResolveCreated(cmd.String("created"))
				if err != nil {
					return err
				}

				return crdctl.Publish(ctx, logger, crdctl.PublishConfig{
					ConfigPath: cmd.String("config"),
					Registry:   cmd.String("registry"),
					Repository: cmd.String("repository"),
					AWSProfile: cmd.String("profile"),
					Created:    created,
				})
			},
		},
	}
}
