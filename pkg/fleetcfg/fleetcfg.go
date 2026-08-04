// Package fleetcfg loads the optional fleet.yaml — the small committed
// file carrying an organization's fleet defaults (see docs/rfc-fleet.md).
// Everything in it is overridable by flags and FLEET_* env vars; a
// missing file is not an error.
package fleetcfg

import (
	"fmt"
	"os"

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

	// Hooks are opaque commands wired by --build — the repo-global
	// fallback for every project.
	Hooks Hooks `yaml:"hooks"`

	// Projects carries per-project overrides, keyed by project name.
	// Deliberately hooks-only: a monorepo builds each project its own
	// way, but this tool still has no project model (see the Non-goals
	// table in docs/rfc-fleet.md).
	Projects map[string]Project `yaml:"projects"`
}

// Cluster is one kubecontext's deploy inputs.
type Cluster struct {
	// Values is OPAQUE: merged into releases as .Values.fleet, verbatim.
	// Organization semantics live in the charts, never in this tool.
	Values map[string]any `yaml:"values"`
}

// Project is one project's overrides. Hooks only, on purpose.
type Project struct {
	Hooks Hooks `yaml:"hooks"`
}

// Hooks are repo-owned commands this tool runs but never interprets.
type Hooks struct {
	// Build is an argv executed by `deploy --build` / `test --build`
	// with the resolved FLEET_* env exported.
	Build []string `yaml:"build"`
}

// BuildHook returns the effective build hook for a project: the project's
// own hooks.build when it sets one, otherwise the repo-global hooks.build.
// An empty project name (or an unknown one) asks for the global hook.
func (c Config) BuildHook(project string) []string {
	if p, ok := c.Projects[project]; ok && len(p.Hooks.Build) > 0 {
		return p.Hooks.Build
	}

	return c.Hooks.Build
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
