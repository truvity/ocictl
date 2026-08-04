package fleettest

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/truvity/ocictl/pkg/kubewho"
)

// Tenant is the tenant identity: everything else a test install names
// (state prefixes, databases, projects) derives from this pair, so the
// two halves are resolved together and never independently.
type Tenant struct {
	// Namespace is the Kubernetes namespace the tenant lives in.
	Namespace string

	// Release is the Helm release name inside that namespace.
	Release string
}

// String renders the tenant as namespace/release.
func (t Tenant) String() string { return t.Namespace + "/" + t.Release }

// Env returns the tenant as KEY=VALUE pairs for a child process — the
// bidirectional FLEET_* contract: read as overrides, always re-exported
// with the RESOLVED values.
func (t Tenant) Env(kubecontext string) []string {
	env := []string{}

	if t.Namespace != "" {
		env = append(env, EnvNamespace+"="+t.Namespace)
	}

	if t.Release != "" {
		env = append(env, EnvRelease+"="+t.Release)
	}

	if kubecontext != "" {
		env = append(env, EnvKubecontext+"="+kubecontext)
	}

	return env
}

// Export sets Env in this process, so in-process consumers (and anything
// they spawn without an explicit Env) see the resolved tenant.
func (t Tenant) Export(kubecontext string) error {
	for _, kv := range t.Env(kubecontext) {
		k, v, _ := strings.Cut(kv, "=")

		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("export %s: %w", k, err)
		}
	}

	return nil
}

// Environment variables read as overrides and re-exported with the
// resolved values.
const (
	EnvNamespace   = "FLEET_NAMESPACE"
	EnvRelease     = "FLEET_RELEASE"
	EnvKubecontext = "FLEET_KUBECONTEXT"
)

// CI environment read for the CI release name. Present by construction on
// a GitHub Actions runner and absent everywhere else, which is what makes
// "am I CI?" a fact rather than a flag.
const (
	EnvCIRunNumber  = "GITHUB_RUN_NUMBER"
	EnvCIRunAttempt = "GITHUB_RUN_ATTEMPT"
)

// Bounds asserted at resolution time rather than discovered in
// production: Helm caps release names, Kubernetes caps namespace labels.
const (
	MaxReleaseLen   = 53
	MaxNamespaceLen = 63
)

// Defaults for the identity-derived namespace. Organization semantics are
// parameters (Options / fleet.yaml); these are only the conventional
// starting point.
const (
	DefaultGroupPrefix       = "emp:"
	DefaultPersonalNamespace = "emp-{slug}"
	DefaultNamespacePrefix   = "it-"
)

// rfc1123Label is the name shape Kubernetes and Helm both require.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Resolve derives the tenant and touches nothing: no namespace is
// created, deleted or even read, no env is written (see Tenant.Export).
// It is the single resolution path — deploy, the test harness and whoami
// all call it, so their answers cannot drift.
//
// Namespace (in order):
//
//  1. Options.Namespace
//  2. FLEET_NAMESPACE
//  3. STANDING: the personal namespace — kubectl auth whoami → the single
//     Options.GroupPrefix group (exactly-one semantics) → the
//     Options.PersonalNamespace template.
//     EPHEMERAL: Options.NamespacePrefix + the git short sha.
//
// Release (in order):
//
//  1. Options.Release
//  2. FLEET_RELEASE
//  3. CI: r{GITHUB_RUN_NUMBER}-a{GITHUB_RUN_ATTEMPT|1}
//  4. Options.App — the standing employee install
//  5. EPHEMERAL only: the resolved namespace (one release per namespace)
func Resolve(ctx context.Context, o Options) (Tenant, error) {
	namespace, err := ResolveNamespace(ctx, o)
	if err != nil {
		return Tenant{}, err
	}

	release, err := resolveRelease(o.withDefaults(), namespace)
	if err != nil {
		return Tenant{}, err
	}

	if err := validName(release, "release", MaxReleaseLen); err != nil {
		return Tenant{}, err
	}

	return Tenant{Namespace: namespace, Release: release}, nil
}

// ResolveNamespace resolves the namespace half of the tenant, for
// callers that need only that (whoami's debug output, a bare
// `kubectl -n`). Resolve is the full path and calls this one.
func ResolveNamespace(ctx context.Context, o Options) (string, error) {
	o = o.withDefaults()

	namespace, err := resolveNamespace(ctx, o)
	if err != nil {
		return "", err
	}

	if err := validName(namespace, "namespace", MaxNamespaceLen); err != nil {
		return "", err
	}

	return namespace, nil
}

func resolveNamespace(ctx context.Context, o Options) (string, error) {
	if o.Namespace != "" {
		return o.Namespace, nil
	}

	if ns := os.Getenv(EnvNamespace); ns != "" {
		return ns, nil
	}

	if o.Ephemeral {
		sha, err := gitShortSHA(ctx)
		if err != nil {
			return "", err
		}

		return o.NamespacePrefix + sha, nil
	}

	// Standing: the cluster's own view of the caller decides. Machine
	// identities never carry the prefixed group, so a robot that reached
	// this line fails crisply instead of impersonating anyone.
	user, err := o.whoAmI()(ctx, kubewho.Options{Kubecontext: o.Kubecontext, Kubeconfig: o.Kubeconfig})
	if err != nil {
		return "", err
	}

	slug, err := kubewho.GroupPrefixed(user, o.GroupPrefix)
	if err != nil {
		return "", err
	}

	return RenderNamespace(o.PersonalNamespace, slug)
}

func resolveRelease(o Options, namespace string) (string, error) {
	if o.Release != "" {
		return o.Release, nil
	}

	if r := os.Getenv(EnvRelease); r != "" {
		return r, nil
	}

	if r, ok := ciRelease(); ok {
		return r, nil
	}

	if o.App != "" {
		return o.App, nil
	}

	if o.Ephemeral {
		// The ephemeral namespace is the tenant; one release per namespace.
		return namespace, nil
	}

	return "", fmt.Errorf(
		"no release: set Options.Release/--release, %s, or Options.App/--app (the standing install's app name); "+
			"CI derives it from %s/%s",
		EnvRelease, EnvCIRunNumber, EnvCIRunAttempt)
}

// ciRelease renders r{run_number}-a{attempt} when the CI environment is
// present. GITHUB_RUN_NUMBER is per-repo-unique by GitHub's semantics and
// the namespace already carries the repo, so the pair is unique.
func ciRelease() (string, bool) {
	run := strings.TrimSpace(os.Getenv(EnvCIRunNumber))
	if run == "" {
		return "", false
	}

	attempt := strings.TrimSpace(os.Getenv(EnvCIRunAttempt))
	if attempt == "" {
		attempt = "1"
	}

	return "r" + run + "-a" + attempt, true
}

// RenderNamespace renders the personal-namespace template with a slug.
// The only substitution is {slug} — kept deliberately dumb.
func RenderNamespace(template, slug string) (string, error) {
	if template == "" {
		return "", fmt.Errorf(
			"no personal-namespace template configured (personalNamespace in fleet.yaml or FLEET_PERSONAL_NAMESPACE)")
	}

	if !strings.Contains(template, "{slug}") {
		return "", fmt.Errorf("personal-namespace template %q must contain {slug}", template)
	}

	return strings.ReplaceAll(template, "{slug}", slug), nil
}

// validName asserts the bound and the RFC1123 label shape both halves of
// the tenant identity must satisfy — failing here, with the offending
// name in hand, beats failing inside helm or the API server.
func validName(name, kind string, maxLen int) error {
	if name == "" {
		return fmt.Errorf("empty %s", kind)
	}

	if len(name) > maxLen {
		return fmt.Errorf("%s %q is %d characters, over the %d-character bound", kind, name, len(name), maxLen)
	}

	if !rfc1123Label.MatchString(name) {
		return fmt.Errorf(
			"%s %q is not a valid RFC1123 label (lowercase alphanumerics and -, starting and ending alphanumeric)",
			kind, name)
	}

	return nil
}
