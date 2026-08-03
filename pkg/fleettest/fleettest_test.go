package fleettest

import "testing"

type fakeM struct{ code int }

func (f fakeM) Run() int { return f.code }

// Run must surface namespace-creation failure as exit 1 without running
// the tests — exercised by pointing kubectl at a context that cannot
// exist. (Full lifecycle is covered by the pilot against a real cluster;
// unit scope here is the failure path and the exit-code plumbing.)
func TestRunFailsClosedWithoutCluster(t *testing.T) {
	code := Run(fakeM{code: 0}, Options{
		Kubecontext: "fleettest-no-such-context",
		Kubeconfig:  t.TempDir() + "/empty-kubeconfig",
	})
	if code != 1 {
		t.Errorf("namespace-creation failure must exit 1, got %d", code)
	}
}
