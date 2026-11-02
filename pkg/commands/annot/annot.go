// Package annot is the annotation flags of the commands that publish a
// manifest: a repeatable --annotation k=v, the standard
// org.opencontainers.image.* keys as named flags, and --annotations-from-git.
//
// Nothing is defaulted: with no flag there is no annotation. Precedence, last
// wins: --annotations-from-git, then the named flags, then --annotation.
package annot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v3"
)

// named maps each named flag to the annotation key it fills.
var named = []struct{ flag, key, usage string }{
	{"source", ocispec.AnnotationSource, "URL of the source repository"},
	{"revision", ocispec.AnnotationRevision, "Source control revision (commit) the artifact was built from"},
	{"licenses", ocispec.AnnotationLicenses, "SPDX licence expression"},
	{"url", ocispec.AnnotationURL, "URL with more information about the artifact"},
	{"documentation", ocispec.AnnotationDocumentation, "URL of the documentation"},
	{"vendor", ocispec.AnnotationVendor, "Name of the distributing entity"},
}

// Flags returns the annotation flags.
func Flags() []cli.Flag {
	flags := []cli.Flag{
		&cli.StringSliceFlag{Name: "annotation", Usage: "Manifest annotation key=value (repeatable); wins over the named flags"},
		&cli.BoolFlag{
			Name:  "annotations-from-git",
			Usage: "Fill " + ocispec.AnnotationSource + " from the origin remote (credentials stripped) and " + ocispec.AnnotationRevision + " from HEAD",
		},
	}

	for _, n := range named {
		flags = append(flags, &cli.StringFlag{Name: n.flag, Usage: n.usage + " (" + n.key + ")"})
	}

	return flags
}

// Resolve returns the manifest annotations the flags ask for, or nil when
// there are none. A URL carrying credentials is refused.
func Resolve(ctx context.Context, cmd *cli.Command) (map[string]string, error) {
	out := map[string]string{}

	if cmd.Bool("annotations-from-git") {
		source, revision, err := gitInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("--annotations-from-git: %w", err)
		}

		out[ocispec.AnnotationSource] = source
		out[ocispec.AnnotationRevision] = revision
	}

	for _, n := range named {
		if v := cmd.String(n.flag); v != "" {
			out[n.key] = v
		}
	}

	for _, kv := range cmd.StringSlice("annotation") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--annotation %q: want key=value", kv)
		}

		out[k] = v
	}

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		if err := refuseCredentials(k, out[k]); err != nil {
			return nil, err
		}
	}

	if len(out) == 0 {
		return nil, nil
	}

	return out, nil
}

// Merge returns base overlaid with over (over wins). Either may be nil.
func Merge(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}

	out := make(map[string]string, len(base)+len(over))

	for k, v := range base {
		out[k] = v
	}

	for k, v := range over {
		out[k] = v
	}

	return out
}

// refuseCredentials rejects a value that is a URL with a user name or
// password in it: an annotation is published with the manifest.
func refuseCredentials(key, value string) error {
	if !strings.Contains(value, "://") {
		return nil
	}

	u, err := url.Parse(value)
	if err != nil || u.User == nil {
		return nil //nolint:nilerr // not a URL with a userinfo; nothing to refuse
	}

	return fmt.Errorf("annotation %s: the URL carries credentials (user info); remove them", key)
}

// gitInfo reads the origin remote and HEAD of the repository in the working
// directory.
func gitInfo(ctx context.Context) (source, revision string, err error) {
	remote, err := git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("no origin remote: %w", err)
	}

	source, err = SourceFromRemote(remote)
	if err != nil {
		return "", "", err
	}

	revision, err = git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("no HEAD commit: %w", err)
	}

	return source, revision, nil
}

func git(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}

	return strings.TrimSpace(string(out)), nil
}

// SourceFromRemote turns a git remote URL into the https URL of the
// repository, with any user info dropped and a trailing .git removed:
//
//	https://user:token@host/org/repo.git  ->  https://host/org/repo
//	ssh://git@host/org/repo.git           ->  https://host/org/repo
//	git@host:org/repo.git                 ->  https://host/org/repo
func SourceFromRemote(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", errors.New("empty remote URL")
	}

	var host, path string

	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			// Never echo the remote: it may carry credentials.
			return "", errors.New("the origin remote is not a parseable URL")
		}

		host, path = u.Hostname(), u.Path

		if u.Port() != "" && (u.Scheme == "http" || u.Scheme == "https") {
			host += ":" + u.Port()
		}
	} else {
		// scp-like: [user@]host:path
		rest := remote
		if _, after, ok := strings.Cut(rest, "@"); ok {
			rest = after
		}

		h, p, ok := strings.Cut(rest, ":")
		if !ok {
			return "", errors.New("the origin remote is neither a URL nor host:path (a local path?)")
		}

		host, path = h, p
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" {
		return "", errors.New("the origin remote names no host or path")
	}

	return "https://" + host + "/" + path, nil
}
