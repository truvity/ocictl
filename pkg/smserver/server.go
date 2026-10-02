package smserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/singleflight"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/truvity/ocictl/pkg/sourcemaps"
)

const maxManifestSize = 1 << 20

// Options are the seams a program or a test may replace.
type Options struct {
	Logger *slog.Logger
	// Clock defaults to time.Now.
	Clock func() time.Time
	// TokenProvider mints ECR logins (mode ecr). Default: AWS
	// ecr:GetAuthorizationToken. When set, repository hosts are not required
	// to look like ECR hosts.
	TokenProvider TokenProvider
	// PlainHTTP talks HTTP to registries. For tests and loopback registries
	// only; it is deliberately not a configuration-file setting.
	PlainHTTP bool
}

// Server is the source-map HTTP handler.
type Server struct {
	cfg     *Config
	log     *slog.Logger
	clock   func() time.Time
	apps    map[string]string // app -> repository
	client  *auth.Client
	plain   bool
	cache   *diskCache
	neg     *negativeCache
	flights singleflight.Group
	metrics metrics
	staging string
	entries string
}

// New validates cfg, prepares the (emptied) cache directory and returns the
// server.
func New(cfg *Config, opts Options) (*Server, error) {
	if err := cfg.setDefaults(); err != nil {
		return nil, err
	}

	if err := cfg.validate(opts.TokenProvider == nil); err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, log: opts.Logger, clock: opts.Clock, plain: opts.PlainHTTP, apps: map[string]string{}}
	if s.log == nil {
		s.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	if s.clock == nil {
		s.clock = time.Now
	}

	hosts := map[string]bool{}

	for _, a := range cfg.Apps {
		repo := cfg.Repository(a)
		s.apps[a.Name] = repo
		host, _, _ := strings.Cut(repo, "/")
		hosts[host] = true
	}

	var ecrTokens TokenProvider

	if cfg.Auth.Mode == AuthECR {
		inner := opts.TokenProvider
		if inner == nil {
			inner = NewAWSECRProvider()
		}

		ecrTokens = newCachingProvider(inner, s.clock)
	}

	cred, err := credentialFunc(cfg, hosts, ecrTokens)
	if err != nil {
		return nil, err
	}

	// A request is waiting on this: retry 5xx briefly, not for seconds.
	transport := retry.NewTransport(nil)
	transport.Policy = func() retry.Policy {
		return &retry.GenericPolicy{Retryable: retry.DefaultPredicate, Backoff: retry.ExponentialBackoff(100*time.Millisecond, 2, 0.2), MaxRetry: 2}
	}
	httpClient := &http.Client{Transport: transport}

	s.client = &auth.Client{Client: httpClient, Cache: auth.NewCache(), Credential: cred}
	s.client.SetUserAgent("smctl")

	// The cache is a cache: start empty rather than trust leftovers.
	if err := os.RemoveAll(cfg.Cache.Dir); err != nil {
		return nil, fmt.Errorf("reset cache dir: %w", err)
	}

	s.staging = filepath.Join(cfg.Cache.Dir, "staging")
	s.entries = filepath.Join(cfg.Cache.Dir, "entries")

	for _, d := range []string{s.staging, s.entries} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	s.cache = newDiskCache(int64(cfg.Cache.MaxSize))
	s.neg = newNegativeCache(cfg.NegativeCache.TTL, cfg.NegativeCache.MaxEntries, s.clock)

	return s, nil
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	m := &s.metrics

	return Stats{
		CacheHits: m.hits.Load(), Loads: m.loads.Load(), LoadFailures: m.failures.Load(),
		Refusals: m.refusals.Load(), Denied: m.denied.Load(), NotFound: m.notFound.Load(), NegativeHits: m.negHits.Load(),
		Evictions: m.evictions.Load(), CacheBytes: s.cache.usedBytes(), NegativeItems: s.neg.len(),
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		_, _ = io.WriteString(w, "ok\n")
		return
	case "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.metrics.write(w, s.Stats())

		return
	}

	rec := &statusWriter{ResponseWriter: w, code: http.StatusOK}
	start := s.clock()

	app, release, outcome := s.serveMap(rec, r)

	s.metrics.request(rec.code)
	s.log.InfoContext(r.Context(), "request",
		slog.String("method", r.Method), slog.String("app", logSafe(app)), slog.String("release", logSafe(release)),
		slog.Int("status", rec.code), slog.String("outcome", outcome), slog.Duration("took", s.clock().Sub(start)))
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) serveMap(w http.ResponseWriter, r *http.Request) (app, release, outcome string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return "", "", "method"
	}

	app, rawRelease, rest, ok := splitPath(r.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)

		return "", "", "badpath"
	}

	if _, known := s.apps[app]; !known {
		http.Error(w, "not found", http.StatusNotFound)

		return app, "", "unknown-app"
	}

	tag, err := sourcemaps.SanitizeTag(rawRelease)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)

		return app, "", "badrelease"
	}

	k := key{app, tag}

	if s.neg.has(k) {
		s.metrics.negHits.Add(1)
		http.Error(w, "not found", http.StatusNotFound)

		return app, tag, "negative"
	}

	dir, hit := s.cache.get(k)
	if hit {
		s.metrics.hits.Add(1)
	} else {
		dir, err = s.load(r.Context(), k)
		if err != nil {
			return app, tag, s.fail(r.Context(), w, k, err)
		}
	}

	if !s.serveFile(w, r, dir, rest) {
		if hit {
			// Evicted between lookup and open: one fresh pull.
			if dir, err = s.load(r.Context(), k); err == nil && s.serveFile(w, r, dir, rest) {
				return app, tag, "reloaded"
			}
		}

		http.Error(w, "not found", http.StatusNotFound)

		return app, tag, "nofile"
	}

	if hit {
		return app, tag, "hit"
	}

	return app, tag, "loaded"
}

func (s *Server) fail(ctx context.Context, w http.ResponseWriter, k key, err error) string {
	var refused *refusedError

	switch {
	case errors.Is(err, errNotFound):
		s.metrics.notFound.Add(1)
		s.neg.add(k)
		http.Error(w, "not found", http.StatusNotFound)

		return "notfound"
	case errors.Is(err, errDenied):
		// ghcr answers an anonymous pull of a missing or private package with
		// 403, so this is usually "no such repository yet". Same answer as a
		// miss, remembered like one, but loud enough to notice a bad login.
		s.metrics.denied.Add(1)
		s.neg.add(k)
		s.log.WarnContext(ctx, "registry denied access", slog.String("app", logSafe(k.app)), slog.String("release", logSafe(k.tag)), slog.Any("error", err))
		http.Error(w, "not found", http.StatusNotFound)

		return "denied"
	case errors.As(err, &refused):
		s.metrics.refusals.Add(1)
		s.neg.add(k)
		s.log.WarnContext(ctx, "artifact refused", slog.String("app", logSafe(k.app)), slog.String("release", logSafe(k.tag)), slog.String("reason", refused.reason))
		http.Error(w, "not found", http.StatusNotFound)

		return "refused"
	default:
		s.metrics.failures.Add(1)
		s.log.ErrorContext(ctx, "registry failure", slog.String("app", logSafe(k.app)), slog.String("release", logSafe(k.tag)), slog.Any("error", err))
		http.Error(w, "registry unavailable", http.StatusBadGateway)

		return "registry-error"
	}
}

// serveFile serves rest from the unpacked directory dir. False means the
// directory or file is not there.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, dir, rest string) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}

	defer root.Close() //nolint:errcheck

	f, err := root.Open(rest)
	if err != nil {
		return false
	}

	defer f.Close() //nolint:errcheck

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	http.ServeContent(w, r, "", time.Time{}, f)

	return true
}

// splitPath parses /<app>/<release>/<rest>.map. rest is slash-separated, free
// of empty, "." and ".." segments, backslashes and NULs.
func splitPath(p string) (app, release, rest string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 3 {
		return "", "", "", false
	}

	app, release = parts[0], parts[1]

	for _, seg := range parts[2:] {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return "", "", "", false
		}
	}

	rest = strings.Join(parts[2:], "/")
	if !strings.HasSuffix(rest, sourcemaps.MapSuffix) || path.Clean(rest) != rest {
		return "", "", "", false
	}

	return app, release, rest, true
}

var (
	errNotFound = errors.New("artifact not found")
	errDenied   = errors.New("registry denied access")
)

// load pulls and unpacks k once for all concurrent callers.
func (s *Server) load(ctx context.Context, k key) (string, error) {
	v, err, _ := s.flights.Do(k.app+"\x00"+k.tag, func() (any, error) {
		// A pull must not die with the one request that started it.
		pull, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.FetchTimeout)
		defer cancel()

		if dir, ok := s.cache.get(k); ok {
			return dir, nil
		}

		s.metrics.loads.Add(1)

		return s.pull(pull, k)
	})
	if err != nil {
		return "", err
	}

	return v.(string), nil
}

func (s *Server) pull(ctx context.Context, k key) (string, error) {
	repo, err := remote.NewRepository(s.apps[k.app])
	if err != nil {
		return "", err
	}

	repo.Client = s.client
	repo.PlainHTTP = s.plain

	desc, rc, err := repo.FetchReference(ctx, k.tag)
	if err != nil {
		return "", classify(err)
	}

	defer rc.Close() //nolint:errcheck

	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return "", refuse("%s is a %s, not an image manifest", k.tag, desc.MediaType)
	}

	if desc.Size > maxManifestSize {
		return "", refuse("manifest is %d bytes", desc.Size)
	}

	raw, err := content.ReadAll(rc, desc)
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}

	layer, err := checkManifest(raw, int64(s.cfg.Limits.MaxLayerSize))
	if err != nil {
		return "", err
	}

	blob, err := repo.Blobs().Fetch(ctx, layer)
	if err != nil {
		return "", classify(err)
	}

	defer blob.Close() //nolint:errcheck

	stage, err := os.MkdirTemp(s.staging, "unpack-*")
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(stage) //nolint:errcheck

	tracked := &errReader{r: blob}
	verified := content.NewVerifyReader(tracked, layer)

	size, err := s.unpackVerified(stage, verified, tracked)
	if err != nil {
		return "", err
	}

	if size > s.cache.max {
		return "", refuse("unpacked size %d exceeds the cache size %d", size, s.cache.max)
	}

	final, err := s.entryDir(k)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", err
	}

	if err := os.RemoveAll(final); err != nil {
		return "", err
	}

	if err := os.Rename(stage, final); err != nil {
		return "", err
	}

	if n := s.cache.add(k, final, size); n > 0 {
		s.metrics.evictions.Add(int64(n))
	}

	return final, nil
}

// entryDir is where k lives on disk. Both segments were validated earlier
// (a configured app name, a sanitised tag); this refuses anything that is not
// a single plain path element anyway, so the join cannot leave entries.
func (s *Server) entryDir(k key) (string, error) {
	for _, seg := range []string{k.app, k.tag} {
		if seg == "" || seg == "." || seg == ".." || seg != filepath.Base(seg) || strings.ContainsAny(seg, "/\\") {
			return "", fmt.Errorf("unsafe cache path element %q", seg)
		}
	}

	return filepath.Join(s.entries, filepath.Base(k.app), filepath.Base(k.tag)), nil
}

// logSafe strips line breaks from a value that came from a request.
func logSafe(v string) string {
	return strings.NewReplacer("\n", "", "\r", "").Replace(v)
}

// unpackVerified unpacks into stage and only then checks the blob digest, so
// nothing from a blob that fails verification is ever kept.
func (s *Server) unpackVerified(stage string, verified *content.VerifyReader, tracked *errReader) (int64, error) {
	root, err := os.OpenRoot(stage)
	if err != nil {
		return 0, err
	}

	defer root.Close() //nolint:errcheck

	_, size, err := unpack(verified, root, s.cfg.Limits)
	if err != nil {
		// A transport failure mid-blob surfaces as a gzip/tar error; it is a
		// registry failure (502), not a verdict on the artifact.
		if tracked.err != nil {
			return 0, fmt.Errorf("read layer: %w", tracked.err)
		}

		return 0, err
	}

	if _, err := io.Copy(io.Discard, verified); err != nil {
		if tracked.err != nil {
			return 0, fmt.Errorf("read layer: %w", tracked.err)
		}

		return 0, refuse("layer read: %v", err)
	}

	if err := verified.Verify(); err != nil {
		return 0, refuse("layer digest: %v", err)
	}

	return size, nil
}

// checkManifest validates a source-map manifest and returns its layer.
func checkManifest(raw []byte, maxLayer int64) (ocispec.Descriptor, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ocispec.Descriptor{}, refuse("manifest is not JSON: %v", err)
	}

	artifactType := m.ArtifactType
	if artifactType == "" {
		// `oras push` without --artifact-type records it as the config type.
		artifactType = m.Config.MediaType
	}

	if artifactType != sourcemaps.ArtifactType {
		return ocispec.Descriptor{}, refuse("artifactType %q, want %q", artifactType, sourcemaps.ArtifactType)
	}

	if len(m.Layers) != 1 {
		return ocispec.Descriptor{}, refuse("%d layers, want exactly 1", len(m.Layers))
	}

	layer := m.Layers[0]
	if layer.MediaType != sourcemaps.LayerMediaType {
		return ocispec.Descriptor{}, refuse("layer media type %q, want %q", layer.MediaType, sourcemaps.LayerMediaType)
	}

	if layer.Size < 0 || layer.Size > maxLayer {
		return ocispec.Descriptor{}, refuse("layer is %d bytes, over the %d cap", layer.Size, maxLayer)
	}

	if err := layer.Digest.Validate(); err != nil || layer.Digest.Algorithm() != digest.SHA256 {
		return ocispec.Descriptor{}, refuse("layer digest %q is not a valid sha256", layer.Digest)
	}

	return layer, nil
}

// classify maps a registry error to errNotFound (the tag or blob is absent)
// or leaves it as an infrastructure failure.
func classify(err error) error {
	var resp *errcode.ErrorResponse

	if errors.Is(err, errdef.ErrNotFound) || (errors.As(err, &resp) && resp.StatusCode == http.StatusNotFound) {
		return fmt.Errorf("%w: %v", errNotFound, err)
	}

	if errors.As(err, &resp) && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("%w: %v", errDenied, err)
	}

	return err
}

// errReader remembers the first read error that is not a clean EOF.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && e.err == nil {
		e.err = err
	}

	return n, err
}
