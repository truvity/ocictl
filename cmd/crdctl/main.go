// Command crdctl is the deprecated name of `ocictl crd`; it stays a thin main
// over the same command definitions, with identical flags and output.
//
// Usage:
//
//	crdctl build --config <crdctl.yaml>
//	crdctl publish --config <crdctl.yaml> --registry <url> --repository <path>
package main

import (
	"os"

	"github.com/truvity/ocictl/pkg/commands/app"
)

var Version = "dev"

func main() {
	args := append([]string{app.Crdctl}, os.Args[1:]...)
	os.Exit(app.Run(args, Version))
}
