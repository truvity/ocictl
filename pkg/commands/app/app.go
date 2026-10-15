// Package app assembles the ocictl command tree and its deprecated aliases.
//
// New is the `ocictl` binary. Legacy is the same code under an old name
// (helmctl, crdctl, smctl): the old tool's commands mounted at the top level,
// with the old tool's error reporting, so flags and output are unchanged.
//
// To add a command group (the next one planned is `release`), write a
// package under pkg/commands that exports Command() and append it to New's
// Commands slice. Nothing else needs to change.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/commands/chartcmd"
	"github.com/truvity/ocictl/pkg/commands/crdcmd"
	"github.com/truvity/ocictl/pkg/commands/smcmd"
	"github.com/truvity/ocictl/pkg/commands/staticcmd"
)

// Legacy names the deprecated binaries a single ocictl binary also answers to.
const (
	Helmctl = "helmctl"
	Crdctl  = "crdctl"
	Smctl   = "smctl"
)

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// New returns the ocictl root command.
func New(version string) *cli.Command {
	logger := newLogger()

	return &cli.Command{
		Name:    "ocictl",
		Usage:   "Deterministic OCI publishing: Helm charts, CRD charts, source maps, static sites",
		Version: version,
		Commands: []*cli.Command{
			{Name: "chart", Usage: "Helm chart packaging and push (was helmctl)", Commands: chartcmd.Commands(logger)},
			{Name: "crd", Usage: "Upstream CRDs to charts (was crdctl)", Commands: crdcmd.Commands(logger)},
			{Name: "sourcemaps", Usage: "Source maps as OCI artifacts (was smctl)", Commands: smcmd.Commands(version)},
			staticcmd.Command(),
			staticcmd.DigestCommand(),
		},
	}
}

// Legacy returns the root command of a deprecated name, or nil for an
// unknown name.
func Legacy(name, version string) *cli.Command {
	logger := newLogger()

	root := &cli.Command{Name: name, Version: version}

	switch name {
	case Helmctl:
		root.Usage = "Deterministic Helm chart packaging and OCI push"
		root.Commands = chartcmd.Commands(logger)
	case Crdctl:
		root.Usage = "Fetch upstream CRDs, repack into Helm charts, and publish to OCI registries"
		root.Commands = crdcmd.Commands(logger)
	case Smctl:
		root.Usage = "Source maps as OCI artifacts: push after a release, serve to Grafana Alloy"
		root.Commands = smcmd.Commands(version)
	default:
		return nil
	}

	return root
}

// Name returns the command name argv[0] selects: a legacy name when the
// binary is invoked as one (with or without an .exe suffix), else "ocictl".
func Name(argv0 string) string {
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")

	switch base {
	case Helmctl, Crdctl, Smctl:
		return base
	}

	return "ocictl"
}

// Run executes the command selected by args[0] and returns the process exit
// code. Error reporting matches each tool as it was: helmctl and crdctl log
// "command failed", smctl prints "smctl: <err>", ocictl logs like helmctl.
func Run(args []string, version string) int {
	name := Name(args[0])

	root := Legacy(name, version)
	if root == nil {
		root = New(version)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err := root.Run(ctx, args)
	if err == nil {
		return 0
	}

	if name == Smctl {
		_, _ = fmt.Fprintln(os.Stderr, "smctl:", err)
	} else {
		newLogger().ErrorContext(ctx, "command failed", slog.Any("error", err))
	}

	return 1
}
