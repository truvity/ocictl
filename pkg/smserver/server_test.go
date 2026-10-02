package smserver_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/truvity/ocictl/pkg/internal/fakeregistry"
	"github.com/truvity/ocictl/pkg/ocipush"
	"github.com/truvity/ocictl/pkg/smserver"
	"github.com/truvity/ocictl/pkg/sourcemaps"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

type env struct {
	reg   *fakeregistry.Registry
	srv   *smserver.Server
	http  *httptest.Server
	clock *clock
	cache string
}

// newEnv starts a fake registry and a server with one app "web" whose
// repository lives in it. tune may adjust the configuration.
func newEnv(t *testing.T, regUser string, tune func(*smserver.Config, *smserver.Options)) *env {
	t.Helper()

	reg := fakeregistry.New(regUser, "secret")
	t.Cleanup(reg.Close)

	e := &env{reg: reg, clock: &clock{t: time.Unix(1_800_000_000, 0)}, cache: filepath.Join(t.TempDir(), "cache")}
	cfg := &smserver.Config{
		RepositoryTemplate: reg.Host() + "/org/sourcemaps/{app}",
		Apps:               []smserver.App{{Name: "web"}},
		Cache:              smserver.Cache{Dir: e.cache},
	}
	opts := smserver.Options{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     e.clock.Now,
		PlainHTTP: true,
	}

	if tune != nil {
		tune(cfg, &opts)
	}

	srv, err := smserver.New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}

	e.srv = srv
	e.http = httptest.NewServer(srv)
	t.Cleanup(e.http.Close)

	return e
}

func (e *env) get(t *testing.T, method, path string) (int, string) {
	t.Helper()

	req, _ := http.NewRequest(method, e.http.URL+path, nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

// publish packs files and pushes them under version through the real push
// path, with credentials the fake registry accepts.
func (e *env) publish(t *testing.T, app, version string, files map[string]string, user string) {
	t.Helper()
	e.publishWith(t, app, version, files, user, "secret")
}

func (e *env) publishWith(t *testing.T, app, version string, files map[string]string, user, password string) {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	packed, err := sourcemaps.PackDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	art, err := sourcemaps.Artifact(packed, version)
	if err != nil {
		t.Fatal(err)
	}

	cred := func(context.Context, string) (auth.Credential, error) {
		return auth.Credential{Username: user, Password: password}, nil
	}

	_, err = sourcemaps.Push(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		e.reg.Host()+"/org/sourcemaps/"+app, art, ocipush.Options{PlainHTTP: true, Credential: cred})
	if err != nil {
		t.Fatal(err)
	}
}

type tarEntry struct {
	hdr  tar.Header
	body string
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for i := range entries {
		e := &entries[i]
		h := e.hdr
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}

		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}

		h.Mode = 0o644

		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}

		if h.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// putRaw stores a hand-made manifest + layer under web:tag.
func (e *env) putRaw(t *testing.T, tag, artifactType, layerType string, layer []byte) {
	t.Helper()

	cfgDigest := e.reg.PutBlob(sourcemaps.EmptyConfig)
	layerDigest := e.reg.PutBlob(layer)
	e.putManifest(t, tag, artifactType, cfgDigest, layerType, layerDigest, int64(len(layer)))
}

func (e *env) putManifest(t *testing.T, tag, artifactType, cfgDigest, layerType, layerDigest string, layerSize int64) {
	t.Helper()

	m := map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"mediaType": sourcemaps.ConfigMediaType, "digest": cfgDigest, "size": 2},
		"layers":        []any{map[string]any{"mediaType": layerType, "digest": layerDigest, "size": layerSize}},
	}
	if artifactType != "" {
		m["artifactType"] = artifactType
	}

	raw, _ := json.Marshal(m)
	e.reg.PutManifest("org/sourcemaps/web", tag, ocispec.MediaTypeImageManifest, raw)
}

func TestHitServesAndCaches(t *testing.T) {
	e := newEnv(t, "", nil)
	e.publish(t, "web", "1.2.3+build.5", map[string]string{
		"assets/index-ab.js.map": `{"version":3,"sources":["a.ts"]}`,
		"main.js.map":            `{"version":3}`,
	}, "")

	code, body := e.get(t, "GET", "/web/1.2.3+build.5/assets/index-ab.js.map")
	if code != 200 || body != `{"version":3,"sources":["a.ts"]}` {
		t.Fatalf("hit: %d %q", code, body)
	}

	if code, _ := e.get(t, "HEAD", "/web/1.2.3+build.5/main.js.map"); code != 200 {
		t.Fatalf("HEAD: %d", code)
	}

	if code, _ := e.get(t, "GET", "/web/1.2.3+build.5/nope.js.map"); code != 404 {
		t.Fatalf("missing file: %d", code)
	}

	if n := e.reg.Count("GET /v2/org/sourcemaps/web/manifests/"); n != 1 {
		t.Fatalf("manifest fetched %d times, want 1 (second request must come from the cache)", n)
	}

	if s := e.srv.Stats(); s.Loads != 1 || s.CacheHits < 2 {
		t.Fatalf("stats %+v", s)
	}
}

func TestMissIsNotFoundThenNegativelyCached(t *testing.T) {
	e := newEnv(t, "", nil)

	for i := 0; i < 3; i++ {
		if code, _ := e.get(t, "GET", "/web/9.9.9/a.js.map"); code != 404 {
			t.Fatalf("request %d: %d", i, code)
		}
	}

	if n := e.reg.Count("/manifests/9.9.9"); n != 1 {
		t.Fatalf("registry asked %d times, want 1", n)
	}

	if s := e.srv.Stats(); s.NotFound != 1 || s.NegativeHits != 2 || s.NegativeItems != 1 {
		t.Fatalf("stats %+v", s)
	}

	// After the TTL the registry is asked again; once the artifact exists it is a hit.
	e.publish(t, "web", "9.9.9", map[string]string{"a.js.map": "{}"}, "")

	if code, _ := e.get(t, "GET", "/web/9.9.9/a.js.map"); code != 404 {
		t.Fatalf("still negatively cached expected, got %d", code)
	}

	e.clock.Advance(31 * time.Second)

	if code, _ := e.get(t, "GET", "/web/9.9.9/a.js.map"); code != 200 {
		t.Fatalf("after TTL: %d", code)
	}
}

func TestUnknownAppNeverTouchesARegistry(t *testing.T) {
	e := newEnv(t, "", nil)

	for _, p := range []string{"/other/1.0.0/a.js.map", "/evil.example/1.0.0/a.js.map", "/..%2Fweb/1.0.0/a.js.map"} {
		if code, _ := e.get(t, "GET", p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}

	if n := e.reg.Count("/"); n != 0 {
		t.Fatalf("registry contacted %d times for unconfigured apps", n)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t, "", nil)

	for _, p := range []string{
		"/web/1.0.0/a.js", "/web/1.0.0/", "/web/1.0.0", "/web/1.0.0/../a.js.map", "/web/1.0.0/%2e%2e/a.js.map",
		"/web/1.0.0/a%5Cb.js.map", "/web/%2Fx/a.js.map", "/web/.hidden/a.js.map", "/web/1.0.0//a.js.map",
	} {
		if code, _ := e.get(t, "GET", p); code != 404 {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}

	if code, _ := e.get(t, "POST", "/web/1.0.0/a.js.map"); code != 405 {
		t.Errorf("POST: %d", code)
	}

	if n := e.reg.Count("/"); n != 0 {
		t.Fatalf("registry contacted for malformed requests: %d", n)
	}

	if code, body := e.get(t, "GET", "/healthz"); code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz: %d %q", code, body)
	}

	if code, body := e.get(t, "GET", "/metrics"); code != 200 || !strings.Contains(body, "smctl_requests_total{code=\"404\"}") {
		t.Errorf("metrics: %d %q", code, body)
	}
}

func TestWrongArtifactTypeIsRefused(t *testing.T) {
	e := newEnv(t, "", nil)
	layer := tarGz(t, []tarEntry{{hdr: tar.Header{Name: "a.js.map"}, body: "{}"}})

	e.putRaw(t, "1.0.0", "application/vnd.example.other.v1", sourcemaps.LayerMediaType, layer)
	e.putRaw(t, "1.0.1", sourcemaps.ArtifactType, "application/octet-stream", layer)

	for _, v := range []string{"1.0.0", "1.0.1"} {
		if code, _ := e.get(t, "GET", "/web/"+v+"/a.js.map"); code != 404 {
			t.Errorf("%s: %d", v, code)
		}
	}

	if s := e.srv.Stats(); s.Refusals != 2 {
		t.Fatalf("stats %+v", s)
	}

	// An artifact whose type is only on the config (oras push without
	// --artifact-type) is accepted when it names ours.
	cfg := e.reg.PutBlob([]byte("{}"))
	layerDigest := e.reg.PutBlob(layer)
	raw, _ := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Config:    ocispec.Descriptor{MediaType: sourcemaps.ArtifactType, Digest: digestOf(cfg), Size: 2},
		Layers:    []ocispec.Descriptor{{MediaType: sourcemaps.LayerMediaType, Digest: digestOf(layerDigest), Size: int64(len(layer))}},
	})
	e.reg.PutManifest("org/sourcemaps/web", "1.0.2", ocispec.MediaTypeImageManifest, raw)

	if code, _ := e.get(t, "GET", "/web/1.0.2/a.js.map"); code != 200 {
		t.Errorf("config-typed artifact: %d", code)
	}
}

func TestOversizeIsRefused(t *testing.T) {
	e := newEnv(t, "", func(c *smserver.Config, _ *smserver.Options) {
		c.Limits = smserver.Limits{MaxLayerSize: 4096, MaxTotalSize: 8192, MaxFileSize: 4096, MaxFiles: 2}
		c.Cache.MaxSize = 16384
	})

	big := strings.Repeat("a", 6000)
	cases := map[string][]tarEntry{
		"file-cap": {{hdr: tar.Header{Name: "a.js.map"}, body: big}},
		"total-cap": {
			{hdr: tar.Header{Name: "a.js.map"}, body: big[:4000]},
			{hdr: tar.Header{Name: "b.js.map"}, body: big[:4000]},
			{hdr: tar.Header{Name: "c.js.map"}, body: "x"},
		},
		"count-cap": {{hdr: tar.Header{Name: "a.js.map"}, body: "1"}, {hdr: tar.Header{Name: "b.js.map"}, body: "1"}, {hdr: tar.Header{Name: "c.js.map"}, body: "1"}},
	}

	for name, entries := range cases {
		e.putRaw(t, name, sourcemaps.ArtifactType, sourcemaps.LayerMediaType, tarGz(t, entries))

		if code, _ := e.get(t, "GET", "/web/"+name+"/a.js.map"); code != 404 {
			t.Errorf("%s: %d", name, code)
		}
	}

	// The compressed layer over its cap is refused before the blob is fetched.
	noise := make([]byte, 8000)
	for i := range noise {
		noise[i] = byte(i * 7919 % 251)
	}

	e.putRaw(t, "layer-cap", sourcemaps.ArtifactType, sourcemaps.LayerMediaType, tarGz(t, []tarEntry{{hdr: tar.Header{Name: "a.js.map"}, body: string(noise)}}))

	if code, _ := e.get(t, "GET", "/web/layer-cap/a.js.map"); code != 404 {
		t.Errorf("layer-cap: %d", code)
	}

	if n := e.reg.Count("/blobs/sha256:"); n > 3*2 {
		t.Errorf("blob traffic %d suggests the oversize layer was fetched", n)
	}

	if s := e.srv.Stats(); s.Refusals != 4 {
		t.Fatalf("stats %+v", s)
	}
}

func TestHostileEntriesAreRefused(t *testing.T) {
	e := newEnv(t, "", nil)
	good := tarEntry{hdr: tar.Header{Name: "ok.js.map"}, body: "{}"}
	cases := map[string]tarEntry{
		"dotdot":    {hdr: tar.Header{Name: "../escape.js.map"}, body: "x"},
		"nested":    {hdr: tar.Header{Name: "a/../../escape.js.map"}, body: "x"},
		"absolute":  {hdr: tar.Header{Name: "/etc/escape.js.map"}, body: "x"},
		"backslash": {hdr: tar.Header{Name: `a\b.js.map`}, body: "x"},
		"symlink":   {hdr: tar.Header{Name: "link.js.map", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		"hardlink":  {hdr: tar.Header{Name: "hard.js.map", Typeflag: tar.TypeLink, Linkname: "ok.js.map"}},
		"device":    {hdr: tar.Header{Name: "dev.js.map", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		"fifo":      {hdr: tar.Header{Name: "fifo.js.map", Typeflag: tar.TypeFifo}},
		"duplicate": good,
	}

	for name, bad := range cases {
		e.putRaw(t, name, sourcemaps.ArtifactType, sourcemaps.LayerMediaType, tarGz(t, []tarEntry{good, bad}))

		if code, body := e.get(t, "GET", "/web/"+name+"/ok.js.map"); code != 404 {
			t.Errorf("%s: %d %q, want the whole artifact refused", name, code, body)
		}
	}

	if s := e.srv.Stats(); s.Refusals != int64(len(cases)) {
		t.Fatalf("refusals %d, want %d", s.Refusals, len(cases))
	}

	// Nothing landed outside the cache's entries directory.
	for _, escaped := range []string{filepath.Join(e.cache, "escape.js.map"), filepath.Join(filepath.Dir(e.cache), "escape.js.map"), "/etc/escape.js.map"} {
		if _, err := os.Stat(escaped); err == nil {
			t.Errorf("%s exists", escaped)
		}
	}

	entries, _ := os.ReadDir(filepath.Join(e.cache, "entries"))
	if len(entries) != 0 {
		t.Errorf("refused artifacts left %d entries behind", len(entries))
	}

	staged, _ := os.ReadDir(filepath.Join(e.cache, "staging"))
	if len(staged) != 0 {
		t.Errorf("staging not cleaned: %d", len(staged))
	}
}

func TestKeepsOnlyMapFiles(t *testing.T) {
	e := newEnv(t, "", nil)
	e.putRaw(t, "1.0.0", sourcemaps.ArtifactType, sourcemaps.LayerMediaType, tarGz(t, []tarEntry{
		{hdr: tar.Header{Name: "dir/", Typeflag: tar.TypeDir}},
		{hdr: tar.Header{Name: "dir/a.js.map"}, body: "{}"},
		{hdr: tar.Header{Name: "dir/a.js"}, body: "secret"},
	}))

	if code, _ := e.get(t, "GET", "/web/1.0.0/dir/a.js.map"); code != 200 {
		t.Fatalf("map: %d", code)
	}

	if _, err := os.Stat(filepath.Join(e.cache, "entries", "web", "1.0.0", "dir", "a.js")); err == nil {
		t.Fatal("non-.map file was unpacked")
	}
}

func TestBlobDigestIsVerified(t *testing.T) {
	e := newEnv(t, "", nil)
	layer := tarGz(t, []tarEntry{{hdr: tar.Header{Name: "a.js.map"}, body: "{}"}})
	e.putRaw(t, "1.0.0", sourcemaps.ArtifactType, sourcemaps.LayerMediaType, layer)

	// Same length, different (but still valid) content.
	forged := tarGz(t, []tarEntry{{hdr: tar.Header{Name: "a.js.map"}, body: "{}"}})
	forged = append([]byte{}, forged...)
	forged[len(forged)-1] ^= 0xff
	e.reg.Corrupt(fakeregistry.Digest(layer), forged)

	if code, _ := e.get(t, "GET", "/web/1.0.0/a.js.map"); code != 404 {
		t.Fatalf("corrupt blob served: %d", code)
	}

	// Content that parses cleanly but is not what the digest says.
	other := tarGz(t, []tarEntry{{hdr: tar.Header{Name: "a.js.map"}, body: "XX"}})
	e.putRaw(t, "1.0.1", sourcemaps.ArtifactType, sourcemaps.LayerMediaType, layer)
	e.reg.Corrupt(fakeregistry.Digest(layer), other)

	if code, _ := e.get(t, "GET", "/web/1.0.1/a.js.map"); code != 404 {
		t.Fatalf("mismatched blob served: %d", code)
	}
}

func TestRegistryFailureIs502AndNotRemembered(t *testing.T) {
	e := newEnv(t, "", nil)
	e.publish(t, "web", "1.0.0", map[string]string{"a.js.map": "{}"}, "")
	e.reg.FailWith(http.StatusInternalServerError)

	if code, _ := e.get(t, "GET", "/web/1.0.0/a.js.map"); code != 502 {
		t.Fatalf("registry 500: %d", code)
	}

	e.reg.FailWith(0)

	if code, _ := e.get(t, "GET", "/web/1.0.0/a.js.map"); code != 200 {
		t.Fatalf("after recovery: %d", code)
	}
}

func TestRegistryDenialIs404AndRemembered(t *testing.T) {
	e := newEnv(t, "", nil)
	e.reg.FailWith(http.StatusForbidden)

	for i := 0; i < 2; i++ {
		if code, _ := e.get(t, "GET", "/web/1.0.0/a.js.map"); code != 404 {
			t.Fatalf("request %d: %d", i, code)
		}
	}

	if s := e.srv.Stats(); s.Denied != 1 || s.NegativeHits != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestConcurrentRequestsPullOnce(t *testing.T) {
	e := newEnv(t, "", nil)
	e.publish(t, "web", "1.0.0", map[string]string{"a.js.map": "{}"}, "")

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if code, _ := e.get(t, "GET", "/web/1.0.0/a.js.map"); code != 200 {
				t.Errorf("status %d", code)
			}
		}()
	}

	wg.Wait()

	if n := e.reg.Count("GET /v2/org/sourcemaps/web/manifests/1.0.0"); n != 1 {
		t.Fatalf("manifest pulled %d times, want 1", n)
	}
}

func TestLRUEvictsWholeReleases(t *testing.T) {
	e := newEnv(t, "", func(c *smserver.Config, _ *smserver.Options) {
		c.Cache.MaxSize = 100
		c.Limits.MaxTotalSize = 100
	})
	body := strings.Repeat("x", 60)

	for _, v := range []string{"1.0.0", "1.0.1"} {
		e.publish(t, "web", v, map[string]string{"a.js.map": body}, "")

		if code, _ := e.get(t, "GET", "/web/"+v+"/a.js.map"); code != 200 {
			t.Fatalf("%s: %d", v, code)
		}
	}

	if s := e.srv.Stats(); s.Evictions != 1 || s.CacheBytes != 60 {
		t.Fatalf("stats %+v", s)
	}

	if _, err := os.Stat(filepath.Join(e.cache, "entries", "web", "1.0.0")); err == nil {
		t.Fatal("evicted release still on disk")
	}

	// The evicted release comes back on demand.
	if code, got := e.get(t, "GET", "/web/1.0.0/a.js.map"); code != 200 || got != body {
		t.Fatalf("reload: %d", code)
	}

	// An artifact bigger than the whole cache is refused.
	e.publish(t, "web", "2.0.0", map[string]string{"a.js.map": strings.Repeat("y", 101)}, "")

	if code, _ := e.get(t, "GET", "/web/2.0.0/a.js.map"); code != 404 {
		t.Fatalf("oversize release: %d", code)
	}
}

type fakeTokens struct {
	mu    sync.Mutex
	calls []string
	clock *clock
	pw    string
}

func (f *fakeTokens) Token(_ context.Context, host string) (smserver.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, host)

	pw := f.pw
	if pw == "" {
		pw = "secret"
	}

	return smserver.Token{Username: "AWS", Password: pw, Expiry: f.clock.Now().Add(12 * time.Hour)}, nil
}

func TestECRTokenIsReusedUntilNearExpiry(t *testing.T) {
	tokens := &fakeTokens{}

	e := newEnv(t, "AWS", func(c *smserver.Config, o *smserver.Options) {
		c.Auth.Mode = smserver.AuthECR
		o.TokenProvider = tokens
	})
	tokens.clock = e.clock

	for _, v := range []string{"1.0.0", "1.0.1"} {
		e.publish(t, "web", v, map[string]string{"a.js.map": "{}"}, "AWS")

		if code, _ := e.get(t, "GET", "/web/"+v+"/a.js.map"); code != 200 {
			t.Fatalf("%s: %d", v, code)
		}
	}

	if len(tokens.calls) != 1 || tokens.calls[0] != e.reg.Host() {
		t.Fatalf("token minted %v, want exactly once for the registry host", tokens.calls)
	}

	// Within the refresh margin of expiry the registry has rotated its
	// password; the 401 makes the client ask for a login again and the
	// provider mints a fresh one rather than replaying the old.
	e.clock.Advance(12*time.Hour - smserver.RefreshMargin + time.Second)
	tokens.mu.Lock()
	tokens.pw = "secret2"
	tokens.mu.Unlock()
	e.reg.SetCredentials("AWS", "secret2")
	e.publishWith(t, "web", "1.0.2", map[string]string{"a.js.map": "{}"}, "AWS", "secret2")

	if code, _ := e.get(t, "GET", "/web/1.0.2/a.js.map"); code != 200 {
		t.Fatalf("after refresh: %d", code)
	}

	if len(tokens.calls) != 2 {
		t.Fatalf("token minted %d times, want 2", len(tokens.calls))
	}

	// A registry that wants credentials and gets none: 502, not a hit.
	anon := newEnv(t, "AWS", nil)
	anon.publish(t, "web", "1.0.0", map[string]string{"a.js.map": "{}"}, "AWS")

	if code, _ := anon.get(t, "GET", "/web/1.0.0/a.js.map"); code != 502 {
		t.Fatalf("anonymous against an authenticated registry: %d", code)
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, fmt.Sprintf("c%d.yaml", len(body)))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		return p
	}

	good := write("repositoryTemplate: ghcr.io/example-org/sourcemaps/{app}\napps:\n  - name: web\ncache:\n  maxSize: 2GiB\nlimits:\n  maxFileSize: 8MiB\n")

	cfg, err := smserver.LoadConfig(good)
	if err != nil || cfg.Cache.MaxSize != 2<<30 || cfg.Limits.MaxFileSize != 8<<20 || cfg.Listen != ":8080" {
		t.Fatalf("good config: %+v %v", cfg, err)
	}

	cfg.Cache.Dir = filepath.Join(dir, "cache-good")

	if _, err := smserver.New(cfg, smserver.Options{}); err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := smserver.LoadConfig(write("apps: [{name: web}]\nbogus: 1\n")); err == nil {
		t.Error("unknown key accepted")
	}

	bad := map[string]*smserver.Config{
		"no apps":        {},
		"bad name":       {Apps: []smserver.App{{Name: "Web/../x", Repository: "ghcr.io/o/r"}}},
		"duplicate":      {Apps: []smserver.App{{Name: "web", Repository: "ghcr.io/o/r"}, {Name: "web", Repository: "ghcr.io/o/r"}}},
		"no repository":  {Apps: []smserver.App{{Name: "web"}}},
		"repo with tag":  {Apps: []smserver.App{{Name: "web", Repository: "ghcr.io/o/r:latest"}}},
		"unknown mode":   {Apps: []smserver.App{{Name: "web", Repository: "ghcr.io/o/r"}}, Auth: smserver.Auth{Mode: "magic"}},
		"ecr non-ecr":    {Apps: []smserver.App{{Name: "web", Repository: "ghcr.io/o/r"}}, Auth: smserver.Auth{Mode: smserver.AuthECR}},
		"docker no path": {Apps: []smserver.App{{Name: "web", Repository: "ghcr.io/o/r"}}, Auth: smserver.Auth{Mode: smserver.AuthDockerConfig}},
		"total over cache": {
			Apps:   []smserver.App{{Name: "web", Repository: "ghcr.io/o/r"}},
			Limits: smserver.Limits{MaxTotalSize: 10 << 20}, Cache: smserver.Cache{MaxSize: 1 << 20},
		},
	}
	for name, c := range bad {
		c.Cache.Dir = filepath.Join(dir, "cache-"+strings.ReplaceAll(name, " ", "-"))

		if _, err := smserver.New(c, smserver.Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	ecrHost := "1111" + "2222" + "3333" + ".dkr.ecr.eu-west-1.amazonaws.com"
	ok := &smserver.Config{
		Apps: []smserver.App{{Name: "web", Repository: ecrHost + "/team/web"}}, Auth: smserver.Auth{Mode: smserver.AuthECR},
		Cache: smserver.Cache{Dir: filepath.Join(dir, "cache-ecr")},
	}

	if _, err := smserver.New(ok, smserver.Options{TokenProvider: &fakeTokens{}}); err != nil {
		t.Errorf("ECR host rejected: %v", err)
	}

	if n, err := smserver.ParseByteSize("64MiB"); err != nil || n != 64<<20 {
		t.Errorf("ParseByteSize: %d %v", n, err)
	}
}

func digestOf(s string) digest.Digest { return digest.Digest(s) }
