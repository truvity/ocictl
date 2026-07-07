package helmctl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// InjectValues deep-merges overlay into the chart's values.yaml, preserving
// existing comments and ordering: maps merge recursively, scalars and
// sequences are replaced, unknown keys are appended. Operates on a temp
// copy of the chart (Package never modifies the source tree).
func InjectValues(chartDir string, overlay map[string]any) error {
	if len(overlay) == 0 {
		return nil
	}

	valuesPath := filepath.Join(chartDir, "values.yaml")

	data, err := os.ReadFile(valuesPath) //nolint:gosec // temp chart copy
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte{}
		} else {
			return fmt.Errorf("read values.yaml: %w", err)
		}
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse values.yaml: %w", err)
	}

	root := documentMapping(&doc)

	var overlayNode yaml.Node
	if err := overlayNode.Encode(overlay); err != nil {
		return fmt.Errorf("encode overlay: %w", err)
	}

	mergeMapping(root, &overlayNode)

	out, err := marshalDocument(&doc, root)
	if err != nil {
		return err
	}

	if err := os.WriteFile(valuesPath, out, 0o644); err != nil { //nolint:gosec // chart values are not sensitive
		return fmt.Errorf("write values.yaml: %w", err)
	}

	return nil
}

// RequireImageDigests verifies that every entry under the top-level
// `images:` map in the chart's values.yaml carries a non-empty digest.
// Guards published charts against mutable references sneaking through.
func RequireImageDigests(chartDir string) error {
	valuesPath := filepath.Join(chartDir, "values.yaml")

	data, err := os.ReadFile(valuesPath) //nolint:gosec // temp chart copy
	if err != nil {
		return fmt.Errorf("read values.yaml: %w", err)
	}

	var values struct {
		Images map[string]struct {
			Repository string `yaml:"repository"`
			Digest     string `yaml:"digest"`
		} `yaml:"images"`
	}

	if err := yaml.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parse values.yaml: %w", err)
	}

	for name, img := range values.Images {
		if img.Digest == "" {
			return fmt.Errorf("images.%s has no digest — refusing to package a mutable image reference (repository %q)",
				name, img.Repository)
		}
	}

	return nil
}

// documentMapping returns the root mapping of a YAML document, creating an
// empty one for empty files.
func documentMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
		return doc.Content[0]
	}

	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

// marshalDocument serializes the document, attaching root when the original
// document was empty.
func marshalDocument(doc *yaml.Node, root *yaml.Node) ([]byte, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{root}
	}

	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("marshal values.yaml: %w", err)
	}

	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close encoder: %w", err)
	}

	return buf.Bytes(), nil
}

// mergeMapping merges src into dst (both MappingNodes): map values merge
// recursively, everything else is replaced; new keys are appended. dst's
// comments and key order are preserved.
func mergeMapping(dst, src *yaml.Node) {
	for i := 0; i < len(src.Content)-1; i += 2 {
		key := src.Content[i]
		value := src.Content[i+1]

		existing := findMappingValue(dst, key.Value)
		if existing == nil {
			dst.Content = append(dst.Content, cloneNode(key), cloneNode(value))
			continue
		}

		if existing.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			mergeMapping(existing, value)
			continue
		}

		replaceNode(existing, value)
	}
}

// findMappingValue returns the value node for key in a MappingNode, or nil.
func findMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}

	return nil
}

// replaceNode overwrites dst's content with src's, keeping dst's position
// (and thus surrounding comments) intact.
func replaceNode(dst, src *yaml.Node) {
	dst.Kind = src.Kind
	dst.Tag = src.Tag
	dst.Value = src.Value
	dst.Style = 0
	dst.Content = src.Content
}

// cloneNode deep-copies a YAML node.
func cloneNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))

	for i, child := range n.Content {
		c.Content[i] = cloneNode(child)
	}

	return &c
}

// newBytesReader avoids importing bytes in manifest.go for one call.
func newBytesReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
