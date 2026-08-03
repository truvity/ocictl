// Package fleetcfg loads the optional fleet.yaml — the small committed
// file carrying an organization's fleet defaults (see docs/rfc-fleet.md).
// Everything in it is overridable by flags and FLEET_* env vars; a
// missing file is not an error.
package fleetcfg

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is the fleet.yaml schema (v0 — may change until the pilot ends).
type Config struct {
	// PersonalNamespace is the template for the default personal
	// namespace, rendered with the identity slug: "emp-{slug}".
	PersonalNamespace string `yaml:"personalNamespace"`

	// GroupPrefix marks the identity entry in the cluster's groups
	// (e.g. "emp:"). Default "emp:".
	GroupPrefix string `yaml:"groupPrefix"`

	// Clusters maps KUBECONTEXT NAMES (kubeconfig stays the cluster
	// registry — no name indirection) to per-cluster deploy inputs.
	Clusters map[string]Cluster `yaml:"clusters"`

	// Hooks are opaque commands wired by --build.
	Hooks Hooks `yaml:"hooks"`
}

// Cluster is one kubecontext's deploy inputs.
type Cluster struct {
	// Values is OPAQUE: merged into releases as .Values.fleet, verbatim.
	// Organization semantics live in the charts, never in this tool.
	Values map[string]any `yaml:"values"`
}

// Hooks are repo-owned commands this tool runs but never interprets.
type Hooks struct {
	// Build is an argv executed by `deploy --build` / `test --build`
	// with the resolved FLEET_* env exported.
	Build []string `yaml:"build"`
}

// DefaultPath is where Load looks when no path is given.
const DefaultPath = "fleet.yaml"

// Load reads the file at path (or DefaultPath). A missing file returns a
// zero Config and no error — the file is optional by design.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}

	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}

	return cfg, nil
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
