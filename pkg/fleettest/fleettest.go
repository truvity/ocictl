// Package fleettest runs work inside a resolved tenant — the
// (namespace, release) pair a test install is named by.
//
// Two modes, one resolution path (see Resolve):
//
//   - STANDING (default): the tenant already exists — an employee's
//     emp-{slug} namespace with the app's release, or a CI namespace with
//     the run's release. The harness creates and deletes NOTHING; it
//     resolves, exports FLEET_*, and runs. This is the safe default: no
//     namespace lifecycle unless a caller explicitly asks for one.
//   - EPHEMERAL (Options.Ephemeral): the pilot's model — create
//     {prefix}{git-sha}, run, tear down. Still a valid generic capability,
//     no longer the fleet's tenancy model.
//
// Namespace lifecycle shells out to kubectl (the abstraction boundary —
// see pkg/kubewho); no client-go, no kubeconfig parsing.
package fleettest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/truvity/ocictl/pkg/kubewho"
)

// Options configure the harness. The zero value is a standing tenant
// resolved entirely from the environment and the cluster's view of the
// caller.
type Options struct {
	// Kubecontext targets a kubeconfig context (empty = current).
	Kubecontext string

	// Kubeconfig overrides the ambient KUBECONFIG.
	Kubeconfig string

	// Namespace overrides namespace resolution entirely (the
	// FLEET_NAMESPACE path).
	Namespace string

	// Release overrides release resolution entirely (the FLEET_RELEASE
	// path).
	Release string

	// App is the application name — the release of a standing employee
	// install, and the last rung of the release ladder.
	App string

	// PersonalNamespace is the identity-derived namespace template
	// (default "emp-{slug}").
	PersonalNamespace string

	// GroupPrefix marks the identity entry in the cluster's groups
	// (default "emp:").
	GroupPrefix string

	// Ephemeral opts into namespace lifecycle: create {prefix}{git-sha}
	// before the work and delete it after. Off by default — a standing
	// tenant is never created or destroyed by this harness.
	Ephemeral bool

	// NamespacePrefix prefixes the ephemeral namespace (default "it-").
	// Ignored unless Ephemeral.
	NamespacePrefix string

	// Keep preserves the ephemeral namespace on exit for debugging.
	Keep bool

	// Timeout bounds the namespace kubectl calls. Default 60s.
	Timeout time.Duration

	// WhoAmI overrides the cluster identity lookup (kubewho.WhoAmI when
	// nil). The seam exists so resolution is testable without a cluster,
	// and so callers that already know the caller pay for one lookup.
	WhoAmI func(ctx context.Context, o kubewho.Options) (kubewho.User, error)
}

// withDefaults fills the conventional values. Idempotent.
func (o Options) withDefaults() Options {
	if o.NamespacePrefix == "" {
		o.NamespacePrefix = DefaultNamespacePrefix
	}

	if o.PersonalNamespace == "" {
		o.PersonalNamespace = DefaultPersonalNamespace
	}

	if o.GroupPrefix == "" {
		o.GroupPrefix = DefaultGroupPrefix
	}

	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}

	return o
}

func (o Options) whoAmI() func(ctx context.Context, o kubewho.Options) (kubewho.User, error) {
	if o.WhoAmI != nil {
		return o.WhoAmI
	}

	return kubewho.WhoAmI
}

// Namespace returns the resolved namespace for in-process consumers, so
// tests avoid env-var spelunking. Empty until Resolve/New/Run ran.
func Namespace() string { return os.Getenv(EnvNamespace) }

// Release returns the resolved release, the other half of the tenant.
func Release() string { return os.Getenv(EnvRelease) }

// Runner is one prepared tenant.
type Runner struct {
	opts   Options
	tenant Tenant
}

// New resolves the tenant, exports the FLEET_* env, and — in ephemeral
// mode only — ensures the namespace exists. Callers must Close; in
// standing mode Close is a deliberate no-op.
func New(ctx context.Context, opts Options) (*Runner, error) {
	opts = opts.withDefaults()

	tenant, err := Resolve(ctx, opts)
	if err != nil {
		return nil, err
	}

	r := &Runner{opts: opts, tenant: tenant}

	if opts.Ephemeral {
		if err := r.ensureNamespace(ctx); err != nil {
			return nil, err
		}
	}

	// In-process env so library consumers (and any subprocess they spawn)
	// see the resolved values without plumbing.
	if err := tenant.Export(opts.Kubecontext); err != nil {
		return nil, err
	}

	return r, nil
}

// Tenant returns the resolved (namespace, release) pair.
func (r *Runner) Tenant() Tenant { return r.tenant }

// Namespace returns the resolved namespace.
func (r *Runner) Namespace() string { return r.tenant.Namespace }

// Release returns the resolved release.
func (r *Runner) Release() string { return r.tenant.Release }

// ensureNamespace is idempotent: an ephemeral namespace left behind by a
// previous run (--keep, a crash, a deliberate re-run) is reused, not a
// hard failure.
func (r *Runner) ensureNamespace(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	ns := r.tenant.Namespace

	if _, err := r.kubectl(ctx, "get", "namespace", ns); err == nil {
		fmt.Fprintf(os.Stderr,
			"fleettest: reusing existing namespace %s (it is deleted on teardown unless Keep is set)\n", ns)

		return nil
	}

	if _, err := r.kubectl(ctx, "create", "namespace", ns); err != nil {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}

	return nil
}

// Close tears the namespace down in ephemeral mode unless Options.Keep
// asked otherwise. A standing tenant owns no lifecycle here: it was not
// created by this harness and is never deleted by it.
// Uses a fresh context: teardown must run even when the work's context
// is already canceled.
func (r *Runner) Close() error {
	if !r.opts.Ephemeral {
		return nil
	}

	if r.opts.Keep {
		fmt.Fprintf(os.Stderr, "fleettest: keeping namespace %s (--keep)\n", r.tenant.Namespace)

		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
	defer cancel()

	if _, err := r.kubectl(ctx, "delete", "namespace", r.tenant.Namespace, "--wait=false"); err != nil {
		return fmt.Errorf("delete namespace %s: %w", r.tenant.Namespace, err)
	}

	return nil
}

// testMain is the subset of *testing.M this package needs — an interface
// so the harness is testable without a real test binary.
type testMain interface{ Run() int }

// Run is the TestMain entry point. Standing by default: resolve, export,
// run, no teardown. With Options.Ephemeral: namespace up, m.Run,
// namespace down.
//
//	func TestMain(m *testing.M) {
//	    os.Exit(fleettest.Run(m, fleettest.Options{
//	        Kubecontext: "devel@oidc",
//	        App:         "url-shortener",
//	    }))
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
	cmd.Env = append(os.Environ(), r.tenant.Env(r.opts.Kubecontext)...)

	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}

	if err != nil {
		return 1, err
	}

	return 0, nil
}

// kubectl runs one kubectl call, returning its combined stderr so callers
// can distinguish "already exists" from "no cluster" without the noise of
// a probe's failure reaching the user's terminal.
func (r *Runner) kubectl(ctx context.Context, args ...string) (string, error) {
	full := []string{}
	if r.opts.Kubecontext != "" {
		full = append(full, "--context", r.opts.Kubecontext)
	}

	full = append(full, args...)

	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "kubectl", full...)
	cmd.Stderr = &stderr

	if r.opts.Kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+r.opts.Kubeconfig)
	}

	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("kubectl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stderr.String(), nil
}

// gitShortSHA derives the ephemeral namespace suffix from the working
// tree's HEAD.
func gitShortSHA(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short=8", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("derive git sha for the namespace suffix: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}
