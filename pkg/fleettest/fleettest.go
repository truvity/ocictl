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

	// Timeout bounds the namespace create/probe kubectl calls — a handful
	// of fast API round-trips. Default DefaultTimeout. Teardown does NOT
	// share this budget; see TeardownTimeout.
	Timeout time.Duration

	// TeardownTimeout bounds the synchronous namespace delete on teardown,
	// and is handed to kubectl as --timeout. Default
	// DefaultTeardownTimeout. Ignored unless Ephemeral.
	//
	// It is an order of magnitude larger than Timeout on purpose: deleting
	// a namespace returns quickly, but the namespace then sits in
	// Terminating until every finalizer on its content (CNPG clusters, ACK
	// resources, NATS accounts) has run. A teardown that returns before
	// that is a lie — the next run of the same commit finds the namespace
	// still Terminating and either mistakes it for reusable or fails its
	// own create.
	TeardownTimeout time.Duration

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
		o.Timeout = DefaultTimeout
	}

	if o.TeardownTimeout == 0 {
		o.TeardownTimeout = DefaultTeardownTimeout
	}

	return o
}

// Budgets for the two kubectl paths, kept apart because they are not the
// same order of magnitude (see Options.TeardownTimeout).
const (
	// DefaultTimeout bounds the create/probe path.
	DefaultTimeout = 60 * time.Second

	// DefaultTeardownTimeout bounds the synchronous delete, sized for a
	// finalizer-heavy namespace rather than for an API round-trip.
	DefaultTeardownTimeout = 10 * time.Minute

	// teardownGrace is the margin the Go context gets on top of the budget
	// handed to kubectl, so kubectl's own --timeout expires FIRST and the
	// caller reads "timed out waiting for the condition" instead of a
	// process killed by a context deadline.
	teardownGrace = 30 * time.Second
)

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

	// created records that THIS runner's `kubectl create namespace`
	// succeeded. Teardown deletes nothing without it: a namespace found
	// already present was somebody else's to begin with.
	created bool

	// namedByCaller records that the namespace NAME came from
	// Options.Namespace or FLEET_NAMESPACE rather than from this harness's
	// own {prefix}{git-sha} derivation. A name the harness did not choose
	// belongs to whoever chose it, so teardown leaves it alone even when
	// this run happened to be the one that created it.
	namedByCaller bool
}

// New resolves the tenant, exports the FLEET_* env, and — in ephemeral
// mode only — ensures the namespace exists. Callers must Close; in
// standing mode Close is a deliberate no-op.
func New(ctx context.Context, opts Options) (*Runner, error) {
	opts = opts.withDefaults()

	// Read before Export writes FLEET_NAMESPACE back: afterwards every
	// namespace looks caller-supplied.
	namedByCaller := opts.Namespace != "" || os.Getenv(EnvNamespace) != ""

	tenant, err := Resolve(ctx, opts)
	if err != nil {
		return nil, err
	}

	r := &Runner{opts: opts, tenant: tenant, namedByCaller: namedByCaller}

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
// hard failure. Only the create branch marks the namespace as this
// runner's to destroy.
func (r *Runner) ensureNamespace(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	ns := r.tenant.Namespace

	if _, err := r.kubectl(ctx, "get", "namespace", ns); err == nil {
		fmt.Fprintf(os.Stderr,
			"fleettest: reusing existing namespace %s (not created by this run, so teardown will leave it)\n", ns)

		return nil
	}

	if _, err := r.kubectl(ctx, "create", "namespace", ns); err != nil {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}

	r.created = true

	return nil
}

// Close tears down the ephemeral namespace — but only one this runner
// both named and created (see keepReason). A standing tenant owns no
// lifecycle here: it was not created by this harness and is never deleted
// by it.
//
// The delete is SYNCHRONOUS. `kubectl delete` waits for the object to be
// gone unless told otherwise, and that wait is the point: a namespace
// whose content carries finalizers stays Terminating well after the API
// accepts the delete, so returning early both hides teardown failures and
// hands the next run of the same commit a namespace that is neither
// present nor absent.
//
// Uses a fresh context: teardown must run even when the work's context is
// already canceled.
func (r *Runner) Close() error {
	if !r.opts.Ephemeral {
		return nil
	}

	if reason := r.keepReason(); reason != "" {
		fmt.Fprintf(os.Stderr, "fleettest: keeping namespace %s (%s)\n", r.tenant.Namespace, reason)

		return nil
	}

	// kubectl gets the budget; the context gets a margin on top, so the
	// failure a stuck namespace produces is kubectl's own timeout message
	// and not a SIGKILL from the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.TeardownTimeout+teardownGrace)
	defer cancel()

	_, err := r.kubectl(ctx, "delete", "namespace", r.tenant.Namespace,
		"--ignore-not-found", "--timeout", r.opts.TeardownTimeout.String())
	if err != nil {
		return fmt.Errorf("delete namespace %s: %w", r.tenant.Namespace, err)
	}

	return nil
}

// keepReason explains why teardown must not delete this namespace, or ""
// when destroying it is this runner's business. Every branch is an
// application of the RFC's rule: never destroy what you did not create.
func (r *Runner) keepReason() string {
	switch {
	case r.opts.Keep:
		return "Keep is set"

	case !r.created:
		return "it already existed and was reused, not created by this run"

	case r.namedByCaller:
		// Created here, but under a name the caller pinned — so its
		// lifecycle is the caller's, and this run leaks it deliberately
		// rather than deleting something it does not own.
		return "the caller named it via Options.Namespace/" + EnvNamespace

	default:
		return ""
	}
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
