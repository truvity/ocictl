// Package fleettest runs work inside an ephemeral Kubernetes namespace:
// create {prefix}{git-sha}, run, tear down. One implementation, two entry
// points — the fleetctl CLI wraps it for any language, and Go tests call
// Run from TestMain.
//
// Namespace lifecycle shells out to kubectl (the abstraction boundary —
// see pkg/kubewho); no client-go, no kubeconfig parsing.
package fleettest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Options configure the harness.
type Options struct {
	// Kubecontext targets a kubeconfig context (empty = current).
	Kubecontext string

	// Kubeconfig overrides the ambient KUBECONFIG.
	Kubeconfig string

	// NamespacePrefix prefixes the ephemeral namespace (default "it-").
	NamespacePrefix string

	// Namespace overrides derivation entirely (the FLEET_NAMESPACE path);
	// when set, the namespace is still created and torn down.
	Namespace string

	// Keep preserves the namespace on exit for debugging.
	Keep bool

	// Timeout bounds namespace creation kubectl calls. Default 60s.
	Timeout time.Duration
}

// EnvNamespace and EnvKubecontext are exported into the harnessed
// process (and set in-process for the library path).
const (
	EnvNamespace   = "FLEET_NAMESPACE"
	EnvKubecontext = "FLEET_KUBECONTEXT"
)

// Runner is one prepared ephemeral namespace.
type Runner struct {
	opts      Options
	namespace string
}

// New derives the namespace, creates it, and sets the FLEET_* process
// env. Callers must Close (or leak deliberately via Options.Keep).
func New(ctx context.Context, opts Options) (*Runner, error) {
	if opts.NamespacePrefix == "" {
		opts.NamespacePrefix = "it-"
	}

	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}

	namespace := opts.Namespace
	if namespace == "" {
		sha, err := gitShortSHA(ctx)
		if err != nil {
			return nil, err
		}

		namespace = opts.NamespacePrefix + sha
	}

	r := &Runner{opts: opts, namespace: namespace}

	if err := r.kubectl(ctx, "create", "namespace", namespace); err != nil {
		return nil, fmt.Errorf("create namespace %s: %w", namespace, err)
	}

	// In-process env so library consumers (and any subprocess they spawn)
	// see the resolved values without plumbing.
	_ = os.Setenv(EnvNamespace, namespace)
	if opts.Kubecontext != "" {
		_ = os.Setenv(EnvKubecontext, opts.Kubecontext)
	}

	return r, nil
}

// Namespace returns the ephemeral namespace name.
func (r *Runner) Namespace() string { return r.namespace }

// Close tears the namespace down unless Options.Keep asked otherwise.
// Uses a fresh context: teardown must run even when the work's context
// is already canceled.
func (r *Runner) Close() error {
	if r.opts.Keep {
		fmt.Fprintf(os.Stderr, "fleettest: keeping namespace %s (--keep)\n", r.namespace)

		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
	defer cancel()

	if err := r.kubectl(ctx, "delete", "namespace", r.namespace, "--wait=false"); err != nil {
		return fmt.Errorf("delete namespace %s: %w", r.namespace, err)
	}

	return nil
}

// testMain is the subset of *testing.M this package needs — an interface
// so the harness is testable without a real test binary.
type testMain interface{ Run() int }

// Run is the TestMain entry point: namespace up, m.Run, namespace down.
//
//	func TestMain(m *testing.M) {
//	    os.Exit(fleettest.Run(m, fleettest.Options{Kubecontext: "devel@oidc"}))
//	}
func Run(m testMain, opts Options) int {
	ctx := context.Background()

	r, err := New(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleettest: %v\n", err)

		return 1
	}

	code := m.Run()

	if err := r.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "fleettest: teardown: %v\n", err)

		if code == 0 {
			code = 1
		}
	}

	return code
}

// Exec runs a command inside the harness (the CLI's `fleetctl test -- ...`
// path): env is exported, exit code passed through.
func (r *Runner) Exec(ctx context.Context, argv []string) (int, error) {
	if len(argv) == 0 {
		return 1, fmt.Errorf("no command given after --")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), EnvNamespace+"="+r.namespace)

	if r.opts.Kubecontext != "" {
		cmd.Env = append(cmd.Env, EnvKubecontext+"="+r.opts.Kubecontext)
	}

	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}

	if err != nil {
		return 1, err
	}

	return 0, nil
}

func (r *Runner) kubectl(ctx context.Context, args ...string) error {
	full := []string{}
	if r.opts.Kubecontext != "" {
		full = append(full, "--context", r.opts.Kubecontext)
	}

	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "kubectl", full...)
	cmd.Stderr = os.Stderr

	if r.opts.Kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+r.opts.Kubeconfig)
	}

	return cmd.Run()
}

// gitShortSHA derives the namespace suffix from the working tree's HEAD.
func gitShortSHA(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short=8", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("derive git sha for the namespace suffix: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}
