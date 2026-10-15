// Command ocictl is the single ocictl binary.
//
// Usage:
//
//	ocictl chart goreleaser-manifest|package|push …   (was helmctl)
//	ocictl crd build|publish …                        (was crdctl)
//	ocictl sourcemaps push|serve …                    (was smctl)
//	ocictl static push --dir <dir> --repository <repo> --tag <ver> …
//	ocictl digest <dir> [--shape artifact|image]
//
// Invoked under an old name (argv[0] helmctl, crdctl or smctl, for example
// through a symlink), it behaves exactly as that tool did. See README.md.
package main

import (
	"os"

	"github.com/truvity/ocictl/pkg/commands/app"
)

var Version = "dev"

func main() {
	os.Exit(app.Run(os.Args, Version))
}
