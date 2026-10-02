package helmctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// depChart is one chart of a dependency tree, read from its archive.
type depChart struct {
	name     string
	values   map[string]any
	deps     []depRef
	children map[string]*depChart // by chart name
}

// depRef is one entry of a Chart.yaml `dependencies:` block.
type depRef struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
}

// key is the values key the dependency is configured under.
func (d depRef) key() string {
	if d.Alias != "" {
		return d.Alias
	}

	return d.Name
}

// requireDependencyImageDigests applies the RequireImageDigests rule to the
// charts the packaged chart pulls in. A dependency's own `images:` defaults,
// overridden by what the parent sets under the dependency's values key (its
// alias, or its name), must carry digests too: a mutable reference does not
// become acceptable by arriving through a subchart.
func requireDependencyImageDigests(chartTmp string) error {
	archives, err := filepath.Glob(filepath.Join(chartTmp, "charts", "*.tgz"))
	if err != nil {
		return fmt.Errorf("list dependency archives: %w", err)
	}

	if len(archives) == 0 {
		return nil
	}

	parentValues, err := readValuesMap(filepath.Join(chartTmp, "values.yaml"))
	if err != nil {
		return err
	}

	parentDeps, err := readChartDeps(filepath.Join(chartTmp, "Chart.yaml"))
	if err != nil {
		return err
	}

	byName := make(map[string]*depChart, len(archives))

	for _, a := range archives {
		data, err := os.ReadFile(a) //nolint:gosec // under the temp copy
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(a), err)
		}

		c, err := loadDepChart(data)
		if err != nil {
			return fmt.Errorf("read dependency %s: %w", filepath.Base(a), err)
		}

		byName[c.name] = c
	}

	return checkDepTree(parentDeps, parentValues, byName, "")
}

// checkDepTree walks the declared dependencies of one chart.
func checkDepTree(deps []depRef, effective map[string]any, charts map[string]*depChart, prefix string) error {
	for _, d := range deps {
		c, ok := charts[d.Name]
		if !ok {
			continue // not vendored (helm package reports a missing dependency)
		}

		sub, _ := effective[d.key()].(map[string]any)
		merged := mergeValues(c.values, sub)
		where := prefix + d.key()

		if err := requireDigestsInMap(merged, where); err != nil {
			return err
		}

		if err := checkDepTree(c.deps, merged, c.children, where+"."); err != nil {
			return err
		}
	}

	return nil
}

// requireDigestsInMap checks the top-level `images:` map of a values tree.
func requireDigestsInMap(values map[string]any, where string) error {
	images, _ := values["images"].(map[string]any)

	names := make([]string, 0, len(images))
	for n := range images {
		names = append(names, n)
	}

	sort.Strings(names)

	for _, n := range names {
		img, _ := images[n].(map[string]any)

		digest, _ := img["digest"].(string)
		if digest == "" {
			repo, _ := img["repository"].(string)

			return fmt.Errorf("dependency %s: images.%s has no digest — refusing to package a mutable image reference (repository %q)",
				where, n, repo)
		}
	}

	return nil
}

// mergeValues deep-merges over into base (maps merge, everything else
// replaces), returning a new map; neither input is modified.
func mergeValues(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}

	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)

		if bok && ook {
			out[k] = mergeValues(bm, om)

			continue
		}

		out[k] = v
	}

	return out
}

func readValuesMap(p string) (map[string]any, error) {
	data, err := os.ReadFile(p) //nolint:gosec // under the temp copy
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read values.yaml: %w", err)
	}

	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse values.yaml: %w", err)
	}

	return m, nil
}

func readChartDeps(p string) ([]depRef, error) {
	data, err := os.ReadFile(p) //nolint:gosec // under the temp copy
	if err != nil {
		return nil, fmt.Errorf("read Chart.yaml: %w", err)
	}

	return parseChartDeps(data)
}

func parseChartDeps(data []byte) ([]depRef, error) {
	var meta struct {
		Name         string   `yaml:"name"`
		Dependencies []depRef `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse Chart.yaml: %w", err)
	}

	return meta.Dependencies, nil
}

// loadDepChart reads a chart archive into memory, recursing into the
// dependency archives (charts/<n>.tgz) and expanded subcharts
// (charts/<n>/) inside it.
func loadDepChart(tgz []byte) (*depChart, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	entries := map[string][]byte{}

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		base := path.Base(hdr.Name)
		if base != "Chart.yaml" && base != "values.yaml" && path.Ext(base) != ".tgz" {
			continue
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", hdr.Name, err)
		}

		entries[hdr.Name] = data
	}

	return buildDepChart(entries, rootPrefix(entries))
}

// rootPrefix finds `<root>/` from the chart's own Chart.yaml (the entry with
// exactly one path separator).
func rootPrefix(entries map[string][]byte) string {
	for name := range entries {
		if path.Base(name) == "Chart.yaml" && path.Dir(name) != "." && path.Dir(path.Dir(name)) == "." {
			return path.Dir(name) + "/"
		}
	}

	return ""
}

// buildDepChart assembles the chart whose files live under prefix.
func buildDepChart(entries map[string][]byte, prefix string) (*depChart, error) {
	chartYAML, ok := entries[prefix+"Chart.yaml"]
	if !ok {
		return nil, fmt.Errorf("no Chart.yaml under %q", prefix)
	}

	var meta struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(chartYAML, &meta); err != nil {
		return nil, fmt.Errorf("parse Chart.yaml: %w", err)
	}

	c := &depChart{name: meta.Name, children: map[string]*depChart{}}

	if v := entries[prefix+"values.yaml"]; len(v) > 0 {
		if err := yaml.Unmarshal(v, &c.values); err != nil {
			return nil, fmt.Errorf("parse values.yaml of %s: %w", c.name, err)
		}
	}

	deps, err := parseChartDeps(chartYAML)
	if err != nil {
		return nil, err
	}

	c.deps = deps

	for name, data := range entries {
		rest, ok := strings.CutPrefix(name, prefix+"charts/")
		if !ok {
			continue
		}

		var child *depChart

		switch {
		case !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".tgz"):
			child, err = loadDepChart(data)
		case strings.Count(rest, "/") == 1 && strings.HasSuffix(rest, "/Chart.yaml"):
			child, err = buildDepChart(entries, prefix+"charts/"+path.Dir(rest)+"/")
		default:
			continue
		}

		if err != nil {
			return nil, err
		}

		c.children[child.name] = child
	}

	return c, nil
}
