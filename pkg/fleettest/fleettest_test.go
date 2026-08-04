package fleettest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeM struct{ code int }

func (f fakeM) Run() int { return f.code }

// Standing mode touches no cluster: pointed at a context that cannot
// exist, Run must still resolve, export and run the tests — and, with a
// kubectl on PATH that would happily answer, must not call it once. That
// silence is the proof that nothing is created or deleted.
func TestRunStandingTouchesNoCluster(t *testing.T) {
	clearFleetEnv(t)

	calls := fakeKubectl(t, stubExists)

	code := Run(fakeM{code: 3}, Options{
		Kubecontext: "fleettest-no-such-context",
		Kubeconfig:  t.TempDir() + "/empty-kubeconfig",
		Namespace:   "emp-otsar",
		App:         "url-shortener",
	})
	if code != 3 {
		t.Errorf("standing Run must pass the test exit code through, got %d", code)
	}

	if Namespace() != "emp-otsar" || Release() != "url-shortener" {
		t.Errorf("Run must export the tenant: namespace=%q release=%q", Namespace(), Release())
	}

	if got := calls(); len(got) != 0 {
		t.Errorf("standing mode must not shell out to kubectl at all, got %v", got)
	}
}

// A standing tenant that cannot be resolved fails closed before m.Run.
func TestRunStandingFailsClosedWithoutRelease(t *testing.T) {
	clearFleetEnv(t)

	ran := false

	code := Run(fakeFuncM(func() int { ran = true; return 0 }), Options{Namespace: "emp-otsar"})
	if code != 1 || ran {
		t.Errorf("unresolvable tenant must exit 1 without running tests (code %d, ran %v)", code, ran)
	}
}

// Run must surface namespace-creation failure as exit 1 without running
// the tests — exercised by pointing kubectl at a context that cannot
// exist. (Full lifecycle is covered by the pilot against a real cluster;
// unit scope here is the failure path and the exit-code plumbing.)
func TestRunEphemeralFailsClosedWithoutCluster(t *testing.T) {
	clearFleetEnv(t)

	code := Run(fakeM{code: 0}, Options{
		Ephemeral:   true,
		Kubecontext: "fleettest-no-such-context",
		Kubeconfig:  t.TempDir() + "/empty-kubeconfig",
		Namespace:   "it-deadbeef",
	})
	if code != 1 {
		t.Errorf("namespace-creation failure must exit 1, got %d", code)
	}
}

type fakeFuncM func() int

func (f fakeFuncM) Run() int { return f() }

// An ephemeral namespace left behind by a previous run must be reused,
// not a hard failure — the pilot's re-run friction. The fake kubectl
// refuses to create, so New can only succeed via the reuse path.
func TestEphemeralReusesExistingNamespace(t *testing.T) {
	clearFleetEnv(t)
	fakeKubectl(t, `case "$*" in
  *"get namespace"*) exit 0 ;;
  *"create namespace"*) echo 'Error from server (AlreadyExists)' >&2; exit 1 ;;
esac
exit 0`)

	r, err := New(context.Background(), Options{Ephemeral: true, Namespace: "it-deadbeef"})
	if err != nil {
		t.Fatalf("an existing namespace must be reused, got %v", err)
	}

	if r.Namespace() != "it-deadbeef" {
		t.Errorf("namespace %q", r.Namespace())
	}

	if r.created {
		t.Error("a reused namespace must not be recorded as created by this run")
	}
}

// The ordinary ephemeral path still creates the namespace.
func TestEphemeralCreatesMissingNamespace(t *testing.T) {
	clearFleetEnv(t)
	fakeKubectl(t, stubMissing)

	r, err := New(context.Background(), Options{Ephemeral: true, Namespace: "it-deadbeef"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !r.created {
		t.Error("the create path must record the namespace as this run's")
	}
}

// The teardown matrix. What a runner may destroy is decided by two facts
// recorded at setup — did THIS run create the namespace, and did the
// harness choose its name — never by the fact that the run merely used
// it. Deleting anything else violates "never destroy what you did not
// create" (docs/rfc-fleet.md).
func TestCloseDeletesOnlyWhatThisRunCreated(t *testing.T) {
	cases := []struct {
		name string

		// env and opts describe the caller; exists describes the cluster
		// as New finds it.
		env    map[string]string
		opts   Options
		exists bool

		// derived namespaces come from {prefix}{git-sha}, so these cases
		// need a git work tree.
		derived bool

		wantDelete bool
		wantReason string
	}{
		{
			name:       "a namespace this run created under its own derived name is ours to delete",
			opts:       Options{Ephemeral: true, NamespacePrefix: "it-"},
			derived:    true,
			wantDelete: true,
		},
		{
			name:       "a reused namespace is left alone",
			opts:       Options{Ephemeral: true, Namespace: "it-deadbeef"},
			exists:     true,
			wantReason: "reused, not created by this run",
		},
		{
			name:       "a caller-named namespace is left alone even when this run created it",
			opts:       Options{Ephemeral: true, Namespace: "it-deadbeef"},
			wantReason: "the caller named it",
		},
		{
			name:       "FLEET_NAMESPACE counts as caller-named",
			env:        map[string]string{EnvNamespace: "it-deadbeef"},
			opts:       Options{Ephemeral: true},
			wantReason: "the caller named it",
		},
		{
			name:       "Keep wins over every other reason",
			opts:       Options{Ephemeral: true, Namespace: "it-deadbeef", Keep: true},
			wantReason: "Keep is set",
		},
		{
			name:       "standing mode never deletes: there was no setup to undo",
			opts:       Options{Namespace: "emp-otsar", App: "url-shortener"},
			exists:     true,
			wantReason: "", // not consulted — Close returns before it
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearFleetEnv(t)

			if tc.derived {
				if _, err := gitShortSHA(context.Background()); err != nil {
					t.Skipf("no git work tree: %v", err)
				}
			}

			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			stub := stubMissing
			if tc.exists {
				stub = stubExists
			}

			calls := fakeKubectl(t, stub)

			r, err := New(context.Background(), tc.opts)
			if err != nil {
				t.Fatalf("new: %v", err)
			}

			if tc.opts.Ephemeral {
				got := r.keepReason()
				if tc.wantReason == "" && got != "" {
					t.Errorf("keepReason() = %q, want no reason to keep", got)
				}

				if !strings.Contains(got, tc.wantReason) {
					t.Errorf("keepReason() = %q, want it to mention %q", got, tc.wantReason)
				}
			}

			if err := r.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			_, deleted := findCall(calls(), "delete namespace")
			if deleted != tc.wantDelete {
				t.Errorf("deleted = %v, want %v (kubectl calls: %v)", deleted, tc.wantDelete, calls())
			}
		})
	}
}

// Item 1's shape: teardown waits for the namespace to actually be gone,
// carries its own kubectl-level --timeout so a finalizer-stuck namespace
// is REPORTED rather than SIGKILLed by a context deadline, tolerates a
// namespace a janitor already reaped — and never borrows the create/probe
// budget.
func TestCloseDeleteIsSynchronousAndSelfBounded(t *testing.T) {
	cases := []struct {
		name        string
		opts        Options
		wantTimeout string
	}{
		{
			name:        "the default teardown budget is sized for finalizers",
			opts:        Options{Ephemeral: true},
			wantTimeout: "10m0s",
		},
		{
			name:        "the create/probe budget does not leak into teardown",
			opts:        Options{Ephemeral: true, Timeout: 5 * time.Second},
			wantTimeout: "10m0s",
		},
		{
			name:        "an explicit teardown budget reaches kubectl",
			opts:        Options{Ephemeral: true, TeardownTimeout: 90 * time.Second},
			wantTimeout: "1m30s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearFleetEnv(t)

			calls := fakeKubectl(t, stubMissing)

			// Constructed directly: this asserts the delete itself, not the
			// ownership derivation the matrix above covers.
			r := &Runner{
				opts:    tc.opts.withDefaults(),
				tenant:  Tenant{Namespace: "it-deadbeef", Release: "it-deadbeef"},
				created: true,
			}

			if err := r.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			del, ok := findCall(calls(), "delete namespace")
			if !ok {
				t.Fatalf("no delete reached kubectl (calls: %v)", calls())
			}

			if strings.Contains(del, "--wait=false") {
				t.Errorf("teardown must block until the namespace is gone: %q", del)
			}

			if !strings.Contains(del, "--ignore-not-found") {
				t.Errorf("a namespace already reaped is not a teardown failure: %q", del)
			}

			if !strings.Contains(del, "--timeout "+tc.wantTimeout) {
				t.Errorf("delete must carry --timeout %s, got %q", tc.wantTimeout, del)
			}
		})
	}
}

// The whole point of waiting: a teardown that could not finish must reach
// the caller as an error rather than as a namespace quietly left in
// Terminating.
func TestCloseReportsTeardownFailure(t *testing.T) {
	clearFleetEnv(t)
	fakeKubectl(t, `case "$*" in
  *"delete namespace"*) echo 'error: timed out waiting for the condition' >&2; exit 1 ;;
esac
exit 0`)

	r := &Runner{
		opts:    Options{Ephemeral: true}.withDefaults(),
		tenant:  Tenant{Namespace: "it-deadbeef", Release: "it-deadbeef"},
		created: true,
	}

	err := r.Close()
	if err == nil {
		t.Fatal("a teardown that timed out must be reported, not swallowed")
	}

	if !strings.Contains(err.Error(), "delete namespace it-deadbeef") ||
		!strings.Contains(err.Error(), "timed out waiting for the condition") {
		t.Errorf("the error must name the namespace and carry kubectl's reason, got %v", err)
	}
}

// The two budgets default independently, and neither one filling in
// disturbs the other.
func TestTimeoutBudgetsAreIndependent(t *testing.T) {
	cases := []struct {
		name             string
		in               Options
		wantTimeout      time.Duration
		wantTeardownWait time.Duration
	}{
		{"both default", Options{}, DefaultTimeout, DefaultTeardownTimeout},
		{"create/probe set", Options{Timeout: time.Second}, time.Second, DefaultTeardownTimeout},
		{"teardown set", Options{TeardownTimeout: time.Hour}, DefaultTimeout, time.Hour},
		{"both set", Options{Timeout: time.Second, TeardownTimeout: time.Hour}, time.Second, time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.Timeout != tc.wantTimeout || got.TeardownTimeout != tc.wantTeardownWait {
				t.Errorf("Timeout=%v TeardownTimeout=%v, want %v / %v",
					got.Timeout, got.TeardownTimeout, tc.wantTimeout, tc.wantTeardownWait)
			}
		})
	}

	// withDefaults is applied twice on the New path (Options.withDefaults
	// then Resolve's own call); the budgets must survive the second pass.
	once := Options{}.withDefaults()

	twice := once.withDefaults()
	if twice.Timeout != once.Timeout || twice.TeardownTimeout != once.TeardownTimeout {
		t.Errorf("withDefaults must be idempotent, got %v / %v", twice.Timeout, twice.TeardownTimeout)
	}
}

// kubectl stubs: the cluster's answer to the `get namespace` probe, which
// is what decides create-vs-reuse. Everything else succeeds.
const (
	stubExists = `case "$*" in
  *"get namespace"*) exit 0 ;;
esac
exit 0`

	stubMissing = `case "$*" in
  *"get namespace"*) echo 'Error from server (NotFound)' >&2; exit 1 ;;
esac
exit 0`
)

// findCall returns the first recorded kubectl argv containing want.
func findCall(calls []string, want string) (string, bool) {
	for _, c := range calls {
		if strings.Contains(c, want) {
			return c, true
		}
	}

	return "", false
}

// fakeKubectl puts a scripted kubectl first on PATH, so the namespace
// lifecycle is testable without a cluster. The returned accessor replays
// the argv of every call the stub received — teardown's contract is as
// much about what it does NOT run as about what it does.
func fakeKubectl(t *testing.T, body string) func() []string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("shell-scripted kubectl stub is POSIX-only")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")

	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		data, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		if err != nil {
			t.Fatalf("read kubectl log: %v", err)
		}

		recorded := strings.TrimSpace(string(data))
		if recorded == "" {
			return nil
		}

		return strings.Split(recorded, "\n")
	}
}
