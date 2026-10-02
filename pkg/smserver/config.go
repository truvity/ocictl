package smserver

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2/registry"
)

// ByteSize is a byte count that YAML may spell as an integer or as a number
// with a KiB/MiB/GiB (or KB/MB/GB, same meaning) suffix.
type ByteSize int64

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	n, err := ParseByteSize(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}

	*b = ByteSize(n)

	return nil
}

// ParseByteSize parses "1048576", "64MiB", "1GiB".
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{{"GiB", 1 << 30}, {"GB", 1 << 30}, {"MiB", 1 << 20}, {"MB", 1 << 20}, {"KiB", 1 << 10}, {"KB", 1 << 10}}
	mult := int64(1)

	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q (want e.g. 1048576, 64MiB)", s)
	}

	return n * mult, nil
}

// Auth modes.
const (
	AuthAnonymous    = "anonymous"
	AuthECR          = "ecr"
	AuthDockerConfig = "dockerConfig"
)

type (
	// Config is the server configuration file.
	Config struct {
		// Listen is the HTTP listen address. Default ":8080".
		Listen string `yaml:"listen"`
		// RepositoryTemplate is host/path with {app}, used for every app
		// that names no repository of its own, e.g.
		// "ghcr.io/example-org/sourcemaps/{app}".
		RepositoryTemplate string `yaml:"repositoryTemplate"`
		// Apps are the applications that may be asked for. Nothing else is.
		Apps []App `yaml:"apps"`
		// Auth selects how the server authenticates to registries.
		Auth Auth `yaml:"auth"`
		// Cache is the on-disk unpack cache.
		Cache Cache `yaml:"cache"`
		// Limits bound what one artifact may unpack to.
		Limits Limits `yaml:"limits"`
		// NegativeCache remembers releases that have no usable artifact.
		NegativeCache NegativeCache `yaml:"negativeCache"`
		// FetchTimeout bounds one registry pull. Default 60s.
		FetchTimeout time.Duration `yaml:"fetchTimeout"`
	}

	// App is one configured application.
	App struct {
		Name string `yaml:"name"`
		// Repository overrides RepositoryTemplate (host/path, no tag).
		Repository string `yaml:"repository"`
	}

	// Auth is the registry authentication.
	Auth struct {
		// Mode is anonymous (default), ecr or dockerConfig.
		Mode string `yaml:"mode"`
		// DockerConfig is the docker config.json path (mode dockerConfig).
		DockerConfig string `yaml:"dockerConfig"`
	}

	// Cache configures the unpack cache.
	Cache struct {
		// Dir is wiped and recreated at start. Default /tmp/smctl-cache.
		Dir string `yaml:"dir"`
		// MaxSize caps the unpacked bytes kept. Default 1GiB.
		MaxSize ByteSize `yaml:"maxSize"`
	}

	// Limits are the per-artifact caps.
	Limits struct {
		// MaxLayerSize caps the compressed layer. Default 256MiB.
		MaxLayerSize ByteSize `yaml:"maxLayerSize"`
		// MaxTotalSize caps the unpacked bytes. Default 512MiB.
		MaxTotalSize ByteSize `yaml:"maxTotalSize"`
		// MaxFileSize caps one unpacked file. Default 64MiB.
		MaxFileSize ByteSize `yaml:"maxFileSize"`
		// MaxFiles caps the kept files. Default 10000.
		MaxFiles int `yaml:"maxFiles"`
	}

	// NegativeCache configures the miss memory.
	NegativeCache struct {
		// TTL is how long a miss is remembered. Default 30s.
		TTL time.Duration `yaml:"ttl"`
		// MaxEntries bounds the memory. Default 4096.
		MaxEntries int `yaml:"maxEntries"`
	}
)

var (
	appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	ecrHostPattern = regexp.MustCompile(`^(\d{12})\.dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)
)

// LoadConfig reads and validates a configuration file. Unknown keys are an
// error.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-provided path
	if err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if err := cfg.setDefaults(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return &cfg, nil
}

func (c *Config) setDefaults() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}

	if c.Auth.Mode == "" {
		c.Auth.Mode = AuthAnonymous
	}

	if c.Cache.Dir == "" {
		c.Cache.Dir = "/tmp/smctl-cache"
	}

	setSize := func(v *ByteSize, def int64) {
		if *v == 0 {
			*v = ByteSize(def)
		}
	}
	setSize(&c.Cache.MaxSize, 1<<30)
	setSize(&c.Limits.MaxLayerSize, 256<<20)
	setSize(&c.Limits.MaxTotalSize, 512<<20)
	setSize(&c.Limits.MaxFileSize, 64<<20)

	if c.Limits.MaxFiles == 0 {
		c.Limits.MaxFiles = 10000
	}

	if c.NegativeCache.TTL == 0 {
		c.NegativeCache.TTL = 30 * time.Second
	}

	if c.NegativeCache.MaxEntries == 0 {
		c.NegativeCache.MaxEntries = 4096
	}

	if c.FetchTimeout == 0 {
		c.FetchTimeout = 60 * time.Second
	}

	return nil
}

// Repository returns the repository (host/path) of an app.
func (c *Config) Repository(a App) string {
	if a.Repository != "" {
		return a.Repository
	}

	return strings.ReplaceAll(c.RepositoryTemplate, "{app}", a.Name)
}

// validate checks everything that does not need the network. requireECRHosts
// is false when a test supplies its own token provider.
func (c *Config) validate(requireECRHosts bool) error {
	if len(c.Apps) == 0 {
		return errors.New("apps: at least one application must be configured")
	}

	switch c.Auth.Mode {
	case AuthAnonymous, AuthECR:
	case AuthDockerConfig:
		if c.Auth.DockerConfig == "" {
			return errors.New("auth.dockerConfig is required for mode dockerConfig")
		}
	default:
		return fmt.Errorf("auth.mode %q: want %s, %s or %s", c.Auth.Mode, AuthAnonymous, AuthECR, AuthDockerConfig)
	}

	if c.Limits.MaxTotalSize > c.Cache.MaxSize {
		return errors.New("limits.maxTotalSize must not exceed cache.maxSize")
	}

	seen := map[string]bool{}

	for _, a := range c.Apps {
		if !appNamePattern.MatchString(a.Name) {
			return fmt.Errorf("apps: name %q must match %s", a.Name, appNamePattern)
		}

		if seen[a.Name] {
			return fmt.Errorf("apps: %q listed twice", a.Name)
		}

		seen[a.Name] = true

		if a.Repository == "" && !strings.Contains(c.RepositoryTemplate, "{app}") {
			return fmt.Errorf("apps[%s]: no repository and repositoryTemplate has no {app}", a.Name)
		}

		repo := c.Repository(a)

		ref, err := registry.ParseReference(repo)
		if err != nil || ref.Reference != "" || ref.Repository == "" {
			return fmt.Errorf("apps[%s]: repository %q must be host/path with no tag or digest", a.Name, repo)
		}

		if c.Auth.Mode == AuthECR && requireECRHosts && !ecrHostPattern.MatchString(ref.Registry) {
			return fmt.Errorf("apps[%s]: auth.mode ecr but %q is not an ECR host", a.Name, ref.Registry)
		}
	}

	return nil
}
