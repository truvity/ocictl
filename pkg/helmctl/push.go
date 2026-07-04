package helmctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/truvity/ocictl/pkg/ocipush"
)

const (
	// helmChartMediaType is the registered IANA media type for Helm chart content layers.
	helmChartMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	// helmConfigMediaType is the registered IANA media type for Helm chart config blobs.
	helmConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
)

type (
	// PushConfig holds configuration for pushing a chart to an OCI registry.
	PushConfig struct {
		// TgzPath is the path to the packaged .tgz file.
		TgzPath string
		// Registry is the OCI registry URL (e.g., "ghcr.io" or "721506300184.dkr.ecr.eu-central-1.amazonaws.com").
		Registry string
		// Repository is the chart path within the registry (e.g., "truvity/charts/cilium-crds").
		Repository string
		// AWSProfile is the AWS profile for ECR authentication (optional, only for ECR).
		AWSProfile string
		// Meta holds chart metadata for the OCI config blob.
		Meta ChartMeta
	}

	// PushResult holds the outcome of a push operation.
	PushResult struct {
		// Ref is the full OCI reference pushed (registry/repo:version).
		Ref string
		// Digest is the manifest digest.
		Digest string
	}

	// ChartMeta holds Chart.yaml metadata used for the OCI config blob and annotations.
	ChartMeta struct {
		Name        string
		Version     string
		Description string
		APIVersion  string
		Type        string
		AppVersion  string
	}

	// helmChartConfig is the config blob structure for Helm OCI charts.
	helmChartConfig struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description,omitempty"`
		APIVersion  string `json:"apiVersion,omitempty"`
		Type        string `json:"type,omitempty"`
		AppVersion  string `json:"appVersion,omitempty"`
	}
)

// Push normalizes the chart .tgz for determinism, then pushes to an OCI registry
// using ORAS (not `helm push`) to produce a reproducible manifest digest.
//
// Determinism guarantees:
//   - tar entries sorted alphabetically, timestamps normalized to epoch 0
//   - OCI manifest has no org.opencontainers.image.created annotation
//   - Same chart content → same layer digest → same manifest digest
func Push(ctx context.Context, logger *slog.Logger, cfg PushConfig) (*PushResult, error) {
	layerData, err := normalizeAndRead(cfg.TgzPath)
	if err != nil {
		return nil, fmt.Errorf("normalize chart: %w", err)
	}

	config := helmChartConfig{
		Name:        cfg.Meta.Name,
		Version:     cfg.Meta.Version,
		Description: cfg.Meta.Description,
		APIVersion:  cfg.Meta.APIVersion,
		Type:        cfg.Meta.Type,
		AppVersion:  cfg.Meta.AppVersion,
	}

	configData, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal chart config: %w", err)
	}

	annotations := map[string]string{
		"org.opencontainers.image.title":   cfg.Meta.Name,
		"org.opencontainers.image.version": cfg.Meta.Version,
	}
	if cfg.Meta.Description != "" {
		annotations["org.opencontainers.image.description"] = cfg.Meta.Description
	}

	repoRef := fmt.Sprintf("%s/%s", cfg.Registry, cfg.Repository)

	artifact := ocipush.Artifact{
		Layer:           layerData,
		LayerMediaType:  helmChartMediaType,
		Config:          configData,
		ConfigMediaType: helmConfigMediaType,
		Tag:             cfg.Meta.Version,
		Annotations:     annotations,
	}

	result, err := ocipush.Push(ctx, logger, repoRef, artifact, cfg.AWSProfile)
	if err != nil {
		return nil, fmt.Errorf("push chart: %w", err)
	}

	return &PushResult{
		Ref:    fmt.Sprintf("%s:%s", repoRef, cfg.Meta.Version),
		Digest: result.Digest,
	}, nil
}

// normalizeAndRead reads a .tgz, repacks with deterministic timestamps and sorted entries.
func normalizeAndRead(tgzPath string) ([]byte, error) {
	origData, err := os.ReadFile(tgzPath)
	if err != nil {
		return nil, fmt.Errorf("read tgz: %w", err)
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(origData))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	type tarEntry struct {
		Header *tar.Header
		Data   []byte
	}

	var entries []tarEntry
	tarReader := tar.NewReader(gzReader)

	for {
		hdr, readErr := tarReader.Next()
		if readErr == io.EOF {
			break
		}

		if readErr != nil {
			return nil, fmt.Errorf("read tar entry: %w", readErr)
		}

		var data []byte
		if hdr.Size > 0 {
			data, err = io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("read tar data for %s: %w", hdr.Name, err)
			}
		}

		entries = append(entries, tarEntry{Header: hdr, Data: data})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Header.Name < entries[j].Header.Name
	})

	epoch := time.Unix(0, 0)
	var buf bytes.Buffer

	gzWriter := gzip.NewWriter(&buf)
	gzWriter.ModTime = epoch
	tarWriter := tar.NewWriter(gzWriter)

	for i := range entries {
		e := &entries[i]
		e.Header.ModTime = epoch
		e.Header.AccessTime = time.Time{}
		e.Header.ChangeTime = time.Time{}
		e.Header.Uid = 0
		e.Header.Gid = 0
		e.Header.Uname = ""
		e.Header.Gname = ""

		if err := tarWriter.WriteHeader(e.Header); err != nil {
			return nil, fmt.Errorf("write header for %s: %w", e.Header.Name, err)
		}

		if len(e.Data) > 0 {
			if _, err := tarWriter.Write(e.Data); err != nil {
				return nil, fmt.Errorf("write data for %s: %w", e.Header.Name, err)
			}
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}

	return buf.Bytes(), nil
}
