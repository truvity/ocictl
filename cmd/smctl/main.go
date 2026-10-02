// Command smctl publishes and serves source maps as OCI artifacts.
//
// Usage:
//
//	smctl push (--goreleaser-dist <dir> --image <name> | --repository <repo> --version <ver>)
//	           --maps <dir> [--include <pattern>...] [--app <name>] [--repository-template <tpl>]
//	           [--profile <aws>] [--dry-run]
//	smctl serve --config <file>
//
// See docs/sourcemaps.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/goreleaserdist"
	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/smserver"
	"github.com/truvity/ocictl/pkg/sourcemaps"
)

var Version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := newApp().Run(ctx, os.Args)

	cancel()

	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "smctl:", err)
		os.Exit(1)
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "smctl",
		Usage:   "Source maps as OCI artifacts: push after a release, serve to Grafana Alloy",
		Version: Version,
		Commands: []*cli.Command{
			{
				Name:  "push",
				Usage: "Pack the .map files of a build into a deterministic OCI artifact and push it under the release version",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "maps", Usage: "Directory holding the .map files (searched recursively)", Required: true},
					&cli.StringSliceFlag{
						Name:  "include",
						Usage: "Also pack non-.map files matching this pattern (path.Match; a pattern without / also tries the base name)",
					},
					&cli.StringFlag{Name: "goreleaser-dist", Usage: "GoReleaser dist directory: takes the version and, with --image, the repository from it"},
					&cli.StringFlag{Name: "image", Usage: "Image published by the release (repository, registry/repository or last segment); needs --goreleaser-dist"},
					&cli.StringFlag{Name: "app", Usage: "Application name for {app} in the template (default: last segment of the image repository)"},
					&cli.StringFlag{
						Name:  "repository-template",
						Usage: "Repository for --image: {registry} {repository} {owner} {app}",
						Value: sourcemaps.DefaultRepositoryTemplate,
					},
					&cli.StringFlag{Name: "repository", Usage: "Explicit repository, host/path with no tag (replaces --image)"},
					&cli.StringFlag{Name: "version", Usage: "Release version (default: the one in --goreleaser-dist)"},
					&cli.StringFlag{Name: "profile", Usage: "AWS profile for ECR auth (optional)"},
					&cli.BoolFlag{Name: "dry-run", Usage: "Pack and print the manifest digest; push nothing"},
				},
				Action: runPush,
			},
			{
				Name:  "serve",
				Usage: "Serve source maps from OCI artifacts to Grafana Alloy's faro.receiver",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Usage: "Configuration file (YAML)", Required: true},
				},
				Action: runServe,
			},
		},
	}
}

func runPush(ctx context.Context, cmd *cli.Command) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	repository, version, err := resolveTarget(cmd)
	if err != nil {
		return err
	}

	packed, err := sourcemaps.PackDir(cmd.String("maps"), cmd.StringSlice("include"))
	if err != nil {
		return err
	}

	artifact, err := sourcemaps.Artifact(packed, version)
	if err != nil {
		return err
	}

	manifestDigest, err := sourcemaps.ManifestDigest(artifact)
	if err != nil {
		return err
	}

	ref := repository + ":" + artifact.Tag

	if cmd.Bool("dry-run") {
		logger.InfoContext(ctx, "dry run: nothing pushed", slog.Int("files", len(packed.Files)), slog.Int("layerBytes", len(packed.Layer)))
		_, _ = fmt.Fprintf(os.Stdout, "%s@%s\n", ref, manifestDigest)

		return nil
	}

	pushed, err := sourcemaps.Push(ctx, logger, repository, artifact, ocipush.Options{AWSProfile: cmd.String("profile")})
	if err != nil {
		return err
	}

	if pushed != manifestDigest {
		return fmt.Errorf("pushed digest %s differs from the computed %s", pushed, manifestDigest)
	}

	_, _ = fmt.Fprintf(os.Stdout, "%s@%s\n", ref, pushed)

	return nil
}

// resolveTarget returns the repository and release version to push.
func resolveTarget(cmd *cli.Command) (repository, version string, err error) {
	repository, version = cmd.String("repository"), cmd.String("version")
	distDir, image := cmd.String("goreleaser-dist"), cmd.String("image")

	var dist *goreleaserdist.Dist

	if distDir != "" {
		if dist, err = goreleaserdist.Load(distDir); err != nil {
			return "", "", err
		}

		if version == "" {
			version = dist.Version
		}
	}

	switch {
	case repository != "" && image != "":
		return "", "", errors.New("--repository and --image are alternatives")
	case repository == "" && image == "":
		return "", "", errors.New("one of --image (with --goreleaser-dist) or --repository is required")
	case repository == "":
		if dist == nil {
			return "", "", errors.New("--image needs --goreleaser-dist")
		}

		img, err := sourcemaps.SelectImage(dist, image)
		if err != nil {
			return "", "", err
		}

		if repository, err = sourcemaps.ExpandRepository(cmd.String("repository-template"), img, cmd.String("app")); err != nil {
			return "", "", err
		}
	}

	if version == "" {
		return "", "", errors.New("--version is required without --goreleaser-dist")
	}

	return repository, version, nil
}

func runServe(ctx context.Context, cmd *cli.Command) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := smserver.LoadConfig(cmd.String("config"))
	if err != nil {
		return err
	}

	handler, err := smserver.New(cfg, smserver.Options{Logger: logger})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)

	go func() { errc <- srv.ListenAndServe() }()

	logger.InfoContext(ctx, "listening", slog.String("addr", cfg.Listen), slog.String("version", Version), slog.Int("apps", len(cfg.Apps)))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return srv.Shutdown(shutdown)
}
