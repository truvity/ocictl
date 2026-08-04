package fleetcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileIsZero(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || cfg.PersonalNamespace != "" {
		t.Errorf("missing file must be zero config, got %+v err %v", cfg, err)
	}
}

func TestLoadFull(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fleet.yaml")
	_ = os.WriteFile(p, []byte(`
personalNamespace: "emp-{slug}"
groupPrefix: "emp:"
clusters:
  devel@oidc:
    values:
      accountID: "123"
      region: eu-central-1
hooks:
  build: ["goreleaser", "release", "--snapshot"]
`), 0o600)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Clusters["devel@oidc"].Values["accountID"] != "123" {
		t.Errorf("cluster values not loaded: %+v", cfg.Clusters)
	}

	if len(cfg.Hooks.Build) != 3 {
		t.Errorf("hooks not loaded: %+v", cfg.Hooks)
	}
}

func TestBuildHookPerProject(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fleet.yaml")
	_ = os.WriteFile(p, []byte(`
hooks:
  build: ["global-build"]
projects:
  url-shortener:
    hooks:
      build: ["moon", "run", "url-shortener:build"]
  no-hooks: {}
`), 0o600)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct {
		project string
		want    string
	}{
		{"url-shortener", "moon run url-shortener:build"}, // project hook wins
		{"no-hooks", "global-build"},                      // declared but hookless → global
		{"unknown", "global-build"},                       // unknown project → global
		{"", "global-build"},                              // no project → global
	}

	for _, tc := range cases {
		if got := strings.Join(cfg.BuildHook(tc.project), " "); got != tc.want {
			t.Errorf("BuildHook(%q) = %q, want %q", tc.project, got, tc.want)
		}
	}
}

func TestBuildHookWithoutGlobal(t *testing.T) {
	cfg := Config{Projects: map[string]Project{"a": {Hooks: Hooks{Build: []string{"a-build"}}}}}

	if got := strings.Join(cfg.BuildHook("a"), " "); got != "a-build" {
		t.Errorf("project hook without a global fallback: %q", got)
	}

	if hook := cfg.BuildHook("b"); len(hook) != 0 {
		t.Errorf("no hook anywhere must be empty, got %v", hook)
	}
}
