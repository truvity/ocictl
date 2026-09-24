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

// RestrictImagesToDeclared narrows an overlay's `images` map to the entries
// the chart actually declares, and removes it entirely from a chart that
// declares no `images` key at all.
//
// A release manifest is built from everything the build produced, but a
// repository commonly publishes several charts from one build — an
// application and the data resources it runs against, say. Without this,
// every chart receives every image: the ones it does not use arrive as
// values it never declared.
//
// That is not cosmetic. A chart whose values.schema.json sets
// `additionalProperties: false` is made UNINSTALLABLE by the extra key,
// and not for one set of values but for every set, including none — the
// schema is checked before any template runs. The chart publishes, the
// release is green, and the failure appears only when somebody installs
// it. A chart that declares no images has nothing a manifest can say
// about it.
//
// The overlay is not modified; a copy is returned.
func RestrictImagesToDeclared(chartDir string, overlay map[string]any) (map[string]any, error) {
	if len(overlay) == 0 {
		return overlay, nil
	}

	if _, ok := overlay["images"]; !ok {
		return overlay, nil
	}

	declared, err := declaredImages(chartDir)
	if err != nil {
		return nil, err
	}

	out := make(map[string]any, len(overlay))
	for k, v := range overlay {
		out[k] = v
	}

	// No `images` key in the chart's own values: it has no images, so the
	// manifest has nothing to contribute and must not add the key.
	if declared == nil {
		delete(out, "images")

		return out, nil
	}

	images, ok := out["images"].(map[string]any)
	if !ok {
		return out, nil
	}

	kept := make(map[string]any, len(images))

	for name, img := range images {
		if declared[name] {
			kept[name] = img
		}
	}

	out["images"] = kept

	return out, nil
}

// declaredImages returns the set of image names under the chart's top-level
// `images:` map, or nil when the chart declares no such key. An empty map
// and a missing one are different answers and the caller acts on each
// differently, which is why this does not collapse them.
func declaredImages(chartDir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(chartDir, "values.yaml")) //nolint:gosec // temp chart copy
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read values.yaml: %w", err)
	}

	var values struct {
		Images map[string]yaml.Node `yaml:"images"`
	}

	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse values.yaml: %w", err)
	}

	if values.Images == nil {
		return nil, nil
	}

	declared := make(map[string]bool, len(values.Images))
	for name := range values.Images {
		declared[name] = true
	}

	return declared, nil
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
