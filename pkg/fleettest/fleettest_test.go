package fleettest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type fakeM struct{ code int }

func (f fakeM) Run() int { return f.code }

// Standing mode touches no cluster: pointed at a context that cannot
// exist, Run must still resolve, export and run the tests — the proof
// that nothing is created or deleted.
func TestRunStandingTouchesNoCluster(t *testing.T) {
	clearFleetEnv(t)

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
}

// The ordinary ephemeral path still creates the namespace.
func TestEphemeralCreatesMissingNamespace(t *testing.T) {
	clearFleetEnv(t)
	fakeKubectl(t, `case "$*" in
  *"get namespace"*) echo 'Error from server (NotFound)' >&2; exit 1 ;;
esac
exit 0`)

	if _, err := New(context.Background(), Options{Ephemeral: true, Namespace: "it-deadbeef"}); err != nil {
		t.Fatalf("create: %v", err)
	}
}

// fakeKubectl puts a scripted kubectl first on PATH, so the namespace
// lifecycle is testable without a cluster.
func fakeKubectl(t *testing.T, body string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("shell-scripted kubectl stub is POSIX-only")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
