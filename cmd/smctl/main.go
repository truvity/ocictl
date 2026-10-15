// Command smctl is the deprecated name of `ocictl sourcemaps`; it stays a thin
// main over the same command definitions, with identical flags and output.
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
	"os"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/commands/app"
)

var Version = "dev"

func main() {
	args := append([]string{app.Smctl}, os.Args[1:]...)
	os.Exit(app.Run(args, Version))
}

// newApp is the smctl command tree (used by the tests).
func newApp() *cli.Command { return app.Legacy(app.Smctl, Version) }
