// Package helmctl implements deterministic Helm chart packaging with
// version/appVersion injection in a TEMP copy (never alters source).
//
// For DISTRIBUTABLE charts only — in-repo components are git-versioned;
// ArgoCD reads from the git path (version = git revision).
package helmctl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

type (
	// PackageConfig holds configuration for chart packaging.
	PackageConfig struct {
		// ChartDir is the source chart directory (never modified).
		ChartDir string
		// Version to inject into Chart.yaml (version + appVersion).
		Version string
		// OutputDir is where the .tgz is written.
		OutputDir string
	}

	// PackageResult holds the result of a package operation.
	PackageResult struct {
		// TgzPath is the absolute path to the packaged .tgz file.
		TgzPath string
		// Name is the chart name from Chart.yaml.
		Name string
		// Version is the injected version.
		Version string
	}
)

// Package copies a chart to a temp dir, injects version/appVersion,
// and runs `helm package`. The source chart directory is NEVER modified.
func Package(ctx context.Context, logger *slog.Logger, cfg PackageConfig) (*PackageResult, error) {
	tmpDir, err := os.MkdirTemp("", "helmctl-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck

	chartName := filepath.Base(cfg.ChartDir)
	chartTmp := filepath.Join(tmpDir, chartName)

	if err := copyDir(cfg.ChartDir, chartTmp); err != nil {
		return nil, fmt.Errorf("copy chart: %w", err)
	}

	if err := InjectChartYAML(chartTmp, cfg.Version); err != nil {
		return nil, fmt.Errorf("inject Chart.yaml: %w", err)
	}

	logger.InfoContext(ctx, "packaging chart",
		slog.String("chart", chartName),
		slog.String("version", cfg.Version),
	)

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir output: %w", err)
	}

	tgzPath, err := helmPackage(ctx, chartTmp, cfg.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("helm package: %w", err)
	}

	return &PackageResult{
		TgzPath: tgzPath,
		Name:    chartName,
		Version: cfg.Version,
	}, nil
}

// InjectChartYAML patches version and appVersion in Chart.yaml.
// Operates on a temp copy (never the source tree).
func InjectChartYAML(chartDir, version string) error {
	chartPath := filepath.Join(chartDir, "Chart.yaml")

	data, err := os.ReadFile(chartPath)
	if err != nil {
		return fmt.Errorf("read Chart.yaml: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse Chart.yaml: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected Chart.yaml structure")
	}

	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return fmt.Errorf("chart.yaml root is not a mapping")
	}

	setYAMLField(mapping, "version", version)
	setYAMLField(mapping, "appVersion", version)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal Chart.yaml: %w", err)
	}

	if err := os.WriteFile(chartPath, out, 0o644); err != nil {
		return fmt.Errorf("write Chart.yaml: %w", err)
	}

	return nil
}

// setYAMLField sets a scalar field in a YAML mapping node.
func setYAMLField(mapping *yaml.Node, key, value string) {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = value
			mapping.Content[i+1].Tag = "!!str"

			return
		}
	}

	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str"},
	)
}

// helmPackage runs `helm package <chartDir> -d <outputDir>` and returns the .tgz path.
func helmPackage(ctx context.Context, chartDir, outputDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "helm", "package", chartDir, "-d", outputDir)
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("helm package: %w", err)
	}

	// helm outputs: "Successfully packaged chart and saved it to: /path/to/chart-1.0.0.tgz"
	const prefix = "Successfully packaged chart and saved it to: "
	for _, l := range splitLines(string(out)) {
		if len(l) > len(prefix) && l[:len(prefix)] == prefix {
			return l[len(prefix):], nil
		}
	}

	// Fallback: find the .tgz in output dir.
	entries, _ := os.ReadDir(outputDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tgz" {
			return filepath.Join(outputDir, e.Name()), nil
		}
	}

	return "", fmt.Errorf("helm package produced no .tgz output")
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range s {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}

	if start < len(s) {
		lines = append(lines, s[start:])
	}

	return lines
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}

		return copyFile(path, dstPath)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)

	return err
}
