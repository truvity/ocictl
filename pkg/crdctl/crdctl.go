// Package crdctl fetches upstream CRDs via the GitHub Contents API at pinned
// versions, caches them locally (gitignored), and repacks into CRD-only Helm
// charts. Optionally packages and pushes to an OCI registry in one shot.
package crdctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/truvity/ocictl/pkg/helmctl"
	"go.yaml.in/yaml/v3"
)

type (
	// Config defines a CRD chart's upstream source and version.
	// Loaded from crdctl.yaml alongside the chart directory.
	Config struct {
		// Repo is the GitHub org/repo (e.g., "cilium/cilium").
		Repo string `yaml:"repo"`

		// Paths is a list of directories within the repo containing CRD YAML files.
		Paths []string `yaml:"paths"`

		// PinnedVersion is the upstream version tag.
		PinnedVersion string `yaml:"pinned_version"`
	}

	// BuildResult holds the outcome of building a CRD chart.
	BuildResult struct {
		Name     string // chart name (derived from config file parent dir)
		Version  string // pinned version (semver without v prefix)
		ChartDir string // output chart directory
	}

	// PublishConfig holds configuration for the full publish pipeline.
	PublishConfig struct {
		// ConfigPath is the path to crdctl.yaml.
		ConfigPath string
		// Registry is the OCI registry (e.g., "ghcr.io").
		Registry string
		// Repository is the chart path (e.g., "truvity/charts/cilium-crds").
		Repository string
		// AWSProfile is optional (only for ECR).
		AWSProfile string
	}

	// githubContentEntry represents a file entry from the GitHub Contents API.
	githubContentEntry struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
		Type        string `json:"type"`
	}
)

var (
	httpClient = &http.Client{Timeout: 60 * time.Second}
)

// LoadConfig reads a crdctl.yaml configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read crdctl.yaml: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse crdctl.yaml: %w", err)
	}

	if cfg.Repo == "" {
		return nil, fmt.Errorf("crdctl.yaml: repo is required")
	}

	if cfg.PinnedVersion == "" {
		return nil, fmt.Errorf("crdctl.yaml: pinned_version is required")
	}

	if len(cfg.Paths) == 0 {
		return nil, fmt.Errorf("crdctl.yaml: paths is required (at least one directory)")
	}

	return &cfg, nil
}

// Build fetches CRDs from GitHub (caching locally), and writes the chart
// templates/crds.yaml into the chart directory (adjacent to crdctl.yaml).
func Build(ctx context.Context, logger *slog.Logger, configPath string) (*BuildResult, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}

	chartDir := filepath.Dir(configPath)
	chartName := filepath.Base(chartDir)
	version := strings.TrimPrefix(cfg.PinnedVersion, "v")

	// Resolve cache dir relative to repo root.
	repoRoot := resolveRepoRoot(configPath)
	cacheDir := filepath.Join(repoRoot, ".cache", "crdctl", chartName, version, "raw")

	if !isCached(cacheDir) {
		logger.InfoContext(ctx, "fetching CRDs from GitHub",
			slog.String("chart", chartName),
			slog.String("version", version),
			slog.String("repo", cfg.Repo),
		)

		crdsData, fetchErr := fetchAllCRDs(ctx, logger, cfg)
		if fetchErr != nil {
			return nil, fmt.Errorf("fetch CRDs: %w", fetchErr)
		}

		if mkErr := os.MkdirAll(cacheDir, 0o755); mkErr != nil {
			return nil, fmt.Errorf("create cache dir: %w", mkErr)
		}

		if wErr := os.WriteFile(filepath.Join(cacheDir, "crds.yaml"), crdsData, 0o644); wErr != nil {
			return nil, fmt.Errorf("write cache: %w", wErr)
		}
	} else {
		logger.InfoContext(ctx, "using cached CRDs",
			slog.String("chart", chartName),
			slog.String("version", version),
		)
	}

	// Write templates/crds.yaml into the chart dir.
	templatesDir := filepath.Join(chartDir, "templates")
	if mkErr := os.MkdirAll(templatesDir, 0o755); mkErr != nil {
		return nil, fmt.Errorf("create templates dir: %w", mkErr)
	}

	cachedData, err := os.ReadFile(filepath.Join(cacheDir, "crds.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read cached CRDs: %w", err)
	}

	if wErr := os.WriteFile(filepath.Join(templatesDir, "crds.yaml"), cachedData, 0o644); wErr != nil {
		return nil, fmt.Errorf("write templates/crds.yaml: %w", wErr)
	}

	return &BuildResult{
		Name:     chartName,
		Version:  version,
		ChartDir: chartDir,
	}, nil
}

// Publish runs the full pipeline: fetch CRDs → build chart → package → push to OCI registry.
func Publish(ctx context.Context, logger *slog.Logger, cfg PublishConfig) error {
	buildResult, err := Build(ctx, logger, cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// Package the chart.
	repoRoot := resolveRepoRoot(cfg.ConfigPath)
	outputDir := filepath.Join(repoRoot, "dist", "crdctl")

	pkgResult, err := helmctl.Package(ctx, logger, helmctl.PackageConfig{
		ChartDir:  buildResult.ChartDir,
		Version:   buildResult.Version,
		OutputDir: outputDir,
	})
	if err != nil {
		return fmt.Errorf("package: %w", err)
	}

	// Push to registry.
	pushResult, err := helmctl.Push(ctx, logger, helmctl.PushConfig{
		TgzPath:    pkgResult.TgzPath,
		Registry:   cfg.Registry,
		Repository: cfg.Repository,
		AWSProfile: cfg.AWSProfile,
		Meta: helmctl.ChartMeta{
			Name:        buildResult.Name,
			Version:     buildResult.Version,
			Description: fmt.Sprintf("CRD definitions for %s", buildResult.Name),
			APIVersion:  "v2",
			Type:        "application",
			AppVersion:  buildResult.Version,
		},
	})
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}

	logger.InfoContext(ctx, "published CRD chart",
		slog.String("name", buildResult.Name),
		slog.String("version", buildResult.Version),
		slog.String("ref", pushResult.Ref),
		slog.String("digest", pushResult.Digest),
	)

	return nil
}

// fetchAllCRDs downloads CRDs from all configured paths and concatenates them.
func fetchAllCRDs(ctx context.Context, logger *slog.Logger, cfg *Config) ([]byte, error) {
	tag := cfg.PinnedVersion
	if tag != "" && tag[0] != 'v' {
		tag = "v" + tag
	}

	var allDocs [][]byte

	for _, path := range cfg.Paths {
		logger.InfoContext(ctx, "listing directory",
			slog.String("repo", cfg.Repo),
			slog.String("path", path),
			slog.String("ref", tag),
		)

		docs, err := fetchCRDsFromDir(ctx, cfg.Repo, path, tag)
		if err != nil {
			return nil, fmt.Errorf("path %s: %w", path, err)
		}

		allDocs = append(allDocs, docs...)
	}

	if len(allDocs) == 0 {
		return nil, fmt.Errorf("no CRD YAML files found in %s@%s", cfg.Repo, tag)
	}

	return bytes.Join(allDocs, []byte("\n---\n")), nil
}

// fetchCRDsFromDir lists a GitHub directory and downloads all .yaml files.
func fetchCRDsFromDir(ctx context.Context, repo, path, ref string) ([][]byte, error) {
	listURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", repo, path, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return nil, fmt.Errorf("GitHub API HTTP %d: %s", resp.StatusCode, string(body))
	}

	var entries []githubContentEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode directory listing: %w", err)
	}

	var docs [][]byte

	for i := range entries {
		entry := &entries[i]

		if entry.Type != "file" {
			continue
		}

		if !strings.HasSuffix(entry.Name, ".yaml") && !strings.HasSuffix(entry.Name, ".yml") {
			continue
		}

		if entry.Name == "kustomization.yaml" || entry.Name == "kustomizeconfig.yaml" {
			continue
		}

		data, dlErr := httpGetWithAuth(ctx, entry.DownloadURL)
		if dlErr != nil {
			return nil, fmt.Errorf("download %s: %w", entry.Name, dlErr)
		}

		docs = append(docs, data)
	}

	return docs, nil
}

// httpGetWithAuth performs an HTTP GET with optional GitHub token auth.
func httpGetWithAuth(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	return io.ReadAll(resp.Body)
}

func isCached(cacheDir string) bool {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return false
	}

	return len(entries) > 0
}

// resolveRepoRoot walks up from configPath to find the git root.
// Falls back to 2 levels up from crdctl.yaml (charts/<name>/crdctl.yaml).
func resolveRepoRoot(configPath string) string {
	abs, _ := filepath.Abs(configPath)
	dir := filepath.Dir(abs)

	// Walk up looking for .git
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
	}

	// Fallback: charts/<name>/crdctl.yaml → repo root is 2 levels up.
	return filepath.Dir(filepath.Dir(dir))
}
