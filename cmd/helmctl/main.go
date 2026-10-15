// Command helmctl is the deprecated name of `ocictl chart`; it stays a thin
// main over the same command definitions so `go run …/cmd/helmctl` and
// `go tool helmctl` keep working, with identical flags and output.
//
// Usage:
//
//	helmctl goreleaser-manifest --goreleaser-dist <dir> [--images a,b] [-o <file>]
//	helmctl package --chart <dir> (--version <ver> | --manifest <file>) [--require-image-digests] --output <dir>
//	helmctl push --tgz <file> --registry <url> --repository <path> [--profile <aws>] --version <ver> --name <name>
//
// See docs/goreleaser.md for the GoReleaser → immutable-chart pipeline.
package main

import (
	"os"

	"github.com/truvity/ocictl/pkg/commands/app"
)

var Version = "dev"

func main() {
	args := append([]string{app.Helmctl}, os.Args[1:]...)
	os.Exit(app.Run(args, Version))
}
