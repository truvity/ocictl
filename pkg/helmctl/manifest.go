package helmctl

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// Manifest is the release manifest consumed by Package: one file carrying
// everything a chart needs to become a deterministic, immutable artifact of
// a specific build — the chart version and the values to bake in (typically
// digest-pinned image references).
//
// Producers: `helmctl goreleaser-manifest` (from a GoReleaser dist/), any
// other build system, or a human. Schema:
//
//	version: 1.6.1          # -> Chart.yaml version (required)
//	appVersion: 1.6.1       # -> Chart.yaml appVersion (default: version)
//	commit: 648e1a2…        # optional, traceability only
//	values:                 # deep-merged into the chart's values.yaml
//	  images:
//	    redirect:
//	      registry: 721…….amazonaws.com
//	      repository: url-shortener/redirect
//	      tag: "1.6.1"
//	      digest: sha256:…
type Manifest struct {
	// Version is injected as Chart.yaml version. Required.
	Version string `yaml:"version"`
	// AppVersion is injected as Chart.yaml appVersion; defaults to Version.
	AppVersion string `yaml:"appVersion,omitempty"`
	// Commit is the source revision the release was built from (optional).
	Commit string `yaml:"commit,omitempty"`
	// Values are deep-merged into the chart's values.yaml: maps merge
	// recursively, scalars and sequences replace.
	Values map[string]any `yaml:"values,omitempty"`
}

// LoadManifest reads and validates a release manifest file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // caller-provided path
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest

	dec := yaml.NewDecoder(newBytesReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}

	return &m, nil
}

// Validate checks required fields and applies defaults.
func (m *Manifest) Validate() error {
	if m.Version == "" {
		return fmt.Errorf("version is required")
	}

	if m.AppVersion == "" {
		m.AppVersion = m.Version
	}

	return nil
}

// Write serializes the manifest to a file (or stdout when path is "-").
func (m *Manifest) Write(path string) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	if path == "-" {
		_, err = os.Stdout.Write(data)

		return err
	}

	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // manifest is not sensitive
		return fmt.Errorf("write manifest: %w", err)
	}

	return nil
}
