package fleetcfg

import (
	"os"
	"path/filepath"
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

func TestRenderNamespace(t *testing.T) {
	if ns, err := RenderNamespace("emp-{slug}", "otsar"); err != nil || ns != "emp-otsar" {
		t.Errorf("got %q err %v", ns, err)
	}

	if _, err := RenderNamespace("", "otsar"); err == nil {
		t.Error("empty template must error")
	}

	if _, err := RenderNamespace("static-ns", "otsar"); err == nil {
		t.Error("template without {slug} must error")
	}
}
