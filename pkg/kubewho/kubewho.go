// Package kubewho answers "who am I, according to the cluster" by shelling
// out to `kubectl auth whoami` — the cluster's own view of the ambient
// credentials.
//
// kubectl is the abstraction boundary on purpose: kubeconfig discovery,
// exec credential plugins (browser SSO, token cache, refresh) and API
// transport are kubectl's job. This package never parses kubeconfig and
// never imports client-go; its single runtime requirement is kubectl in
// PATH (stable since Kubernetes 1.28, SelfSubjectReview).
package kubewho

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Options select which cluster answers. Zero values defer to the ambient
// environment (current kubeconfig context).
type Options struct {
	// Kubecontext is the kubeconfig context name (e.g. "devel@oidc").
	// Empty = kubectl's current context.
	Kubecontext string

	// Kubeconfig overrides the ambient KUBECONFIG for the spawned kubectl.
	Kubeconfig string
}

// User is the cluster's view of the caller.
type User struct {
	// Username as the API server mapped it (e.g. an email).
	Username string

	// Groups verbatim from the API server, including provider-mapped
	// claim entries and system: groups.
	Groups []string
}

// selfSubjectReview is the JSON shape of `kubectl auth whoami -o json`.
type selfSubjectReview struct {
	Status struct {
		UserInfo struct {
			Username string   `json:"username"`
			Groups   []string `json:"groups"`
		} `json:"userInfo"`
	} `json:"status"`
}

// WhoAmI runs `kubectl auth whoami -o json` and returns the caller's
// identity. Interactive credential flows (a first-time browser login) are
// passed through: the subprocess inherits stderr and stdin, and only
// stdout is parsed.
func WhoAmI(ctx context.Context, o Options) (User, error) {
	args := []string{}
	if o.Kubecontext != "" {
		args = append(args, "--context", o.Kubecontext)
	}

	args = append(args, "auth", "whoami", "-o", "json")

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if o.Kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+o.Kubeconfig)
	}

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return User{}, fmt.Errorf("kubectl auth whoami (context %q): %w", o.Kubecontext, err)
	}

	return parseWhoAmI(stdout.Bytes())
}

// parseWhoAmI decodes the SelfSubjectReview JSON. Split from WhoAmI for
// testability without a cluster.
func parseWhoAmI(data []byte) (User, error) {
	var review selfSubjectReview
	if err := json.Unmarshal(data, &review); err != nil {
		return User{}, fmt.Errorf("parse kubectl auth whoami output: %w", err)
	}

	if review.Status.UserInfo.Username == "" {
		return User{}, fmt.Errorf("kubectl auth whoami returned no username — not authenticated?")
	}

	return User{
		Username: review.Status.UserInfo.Username,
		Groups:   review.Status.UserInfo.Groups,
	}, nil
}

// GroupPrefixed returns the single group entry carrying the given prefix,
// with the prefix stripped — exactly-one semantics: zero matches and
// multiple matches are both errors, never guesses. Identity is not a
// place to be forgiving.
func GroupPrefixed(u User, prefix string) (string, error) {
	var found []string

	for _, g := range u.Groups {
		if strings.HasPrefix(g, prefix) {
			found = append(found, strings.TrimPrefix(g, prefix))
		}
	}

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf(
			"no %q group in the cluster identity for %s — the token may predate the identity claim; "+
				"re-run any kubectl command to refresh the login",
			prefix, u.Username)
	default:
		return "", fmt.Errorf("multiple %q groups in the cluster identity for %s: %v — refusing to guess",
			prefix, u.Username, found)
	}
}
