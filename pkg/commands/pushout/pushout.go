// Package pushout is the machine-readable result of the commands that publish
// a manifest: the common --output, --digest-file and --github-output flags and
// the one JSON shape they share.
//
// Without the flags a command prints exactly what it always printed; this
// package only adds to that.
package pushout

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v3"

	"github.com/truvity/ocictl/pkg/helmctl"
	"github.com/truvity/ocictl/pkg/ocipush"
)

// Values of Result.Status.
const (
	// StatusPushed means the tag was absent and has been written.
	StatusPushed = "pushed"
	// StatusAlreadyPresent means the tag already pointed at this exact manifest.
	StatusAlreadyPresent = "already-present"
	// StatusDryRun means nothing was pushed; the digest is the one a push would give.
	StatusDryRun = "dry-run"
)

// Output formats of --output.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Result is the JSON document `--output json` prints for a push command.
type Result struct {
	// Ref is the digest-pinned reference <repository>:<tag>@<digest>.
	Ref string `json:"ref"`
	// Repository is host/path, no tag.
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	// Digest is the manifest digest, sha256:<hex>.
	Digest string `json:"digest"`
	// Status is StatusPushed, StatusAlreadyPresent or StatusDryRun.
	Status string `json:"status"`
	// ArtifactType is the manifest artifactType or, when the manifest has
	// none, its config media type (what a registry client lists the artifact
	// as); empty for an image whose config is a plain empty JSON.
	ArtifactType string `json:"artifactType"`
	// MediaType is the manifest media type.
	MediaType string `json:"mediaType"`
}

// DigestResult is the JSON document `--output json` prints for `digest`.
type DigestResult struct {
	// Digest is the manifest digest.
	Digest       string `json:"digest"`
	LayerDigest  string `json:"layerDigest"`
	ConfigDigest string `json:"configDigest"`
	ArtifactType string `json:"artifactType"`
	MediaType    string `json:"mediaType"`
}

// DigestFlags are the flags every command that has a manifest digest takes.
func DigestFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "output", Usage: "Result format on stdout: text (default, unchanged) or json", Value: FormatText},
		&cli.StringFlag{Name: "digest-file", Usage: "Write the bare manifest digest (sha256:...) and a newline to this file"},
	}
}

// Flags are DigestFlags plus --github-output, for the push commands.
func Flags() []cli.Flag {
	return append(DigestFlags(), &cli.StringFlag{
		Name:  "github-output",
		Usage: "With $GITHUB_OUTPUT set, append <prefix>digest=... and <prefix>ref=... to it (the prefix is used as given)",
	})
}

// Validate checks the flag values. Call it before doing any work, so a typo
// in --output does not cost a push.
func Validate(cmd *cli.Command) error {
	switch f := cmd.String("output"); f {
	case "", FormatText, FormatJSON:
		return nil
	default:
		return fmt.Errorf("--output %q: want %q or %q", f, FormatText, FormatJSON)
	}
}

// New builds the Result of a push (or dry run) of art to repository.
func New(repository string, art ocipush.Artifact, digest, status string) Result {
	return Result{
		Ref:          fmt.Sprintf("%s:%s@%s", repository, art.Tag, digest),
		Repository:   repository,
		Tag:          art.Tag,
		Digest:       digest,
		Status:       status,
		ArtifactType: ocipush.EffectiveArtifactType(art),
		MediaType:    ocispec.MediaTypeImageManifest,
	}
}

// Chart builds the Result of a chart push.
func Chart(res *helmctl.PushResult, status string) Result {
	return Result{
		Ref:          fmt.Sprintf("%s:%s@%s", res.Repository, res.Tag, res.Digest),
		Repository:   res.Repository,
		Tag:          res.Tag,
		Digest:       res.Digest,
		Status:       status,
		ArtifactType: res.ArtifactType,
		MediaType:    ocispec.MediaTypeImageManifest,
	}
}

// Emit reports a Result: the text form (the caller's, unchanged from before
// these flags existed) or JSON on out, then --digest-file and --github-output.
// text prints the command's historical output.
func Emit(out io.Writer, cmd *cli.Command, res Result, text func(io.Writer) error) error {
	if cmd.String("output") == FormatJSON {
		if err := writeJSON(out, res); err != nil {
			return err
		}
	} else if err := text(out); err != nil {
		return err
	}

	if err := writeDigestFile(cmd.String("digest-file"), res.Digest); err != nil {
		return err
	}

	return writeGitHubOutput(cmd.String("github-output"), res)
}

// EmitDigest is Emit for `digest`: DigestResult as JSON, no GitHub output.
func EmitDigest(out io.Writer, cmd *cli.Command, res DigestResult, text func(io.Writer) error) error {
	if cmd.String("output") == FormatJSON {
		if err := writeJSON(out, res); err != nil {
			return err
		}
	} else if err := text(out); err != nil {
		return err
	}

	return writeDigestFile(cmd.String("digest-file"), res.Digest)
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	_, err = fmt.Fprintf(w, "%s\n", data)

	return err
}

func writeDigestFile(path, digest string) error {
	if path == "" {
		return nil
	}

	if err := os.WriteFile(path, []byte(digest+"\n"), 0o644); err != nil { //nolint:gosec // a digest is not a secret
		return fmt.Errorf("--digest-file: %w", err)
	}

	return nil
}

// writeGitHubOutput appends the step outputs to $GITHUB_OUTPUT. Without the
// variable (a run outside GitHub Actions) it warns and writes nothing.
func writeGitHubOutput(prefix string, res Result) error {
	if prefix == "" {
		return nil
	}

	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		slog.Warn("--github-output given but GITHUB_OUTPUT is not set: nothing written")

		return nil
	}

	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("GITHUB_OUTPUT: %w", err)
	}

	_, werr := fmt.Fprintf(f, "%sdigest=%s\n%sref=%s\n", prefix, res.Digest, prefix, res.Ref)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}

	if werr != nil {
		return fmt.Errorf("GITHUB_OUTPUT: %w", werr)
	}

	return nil
}
