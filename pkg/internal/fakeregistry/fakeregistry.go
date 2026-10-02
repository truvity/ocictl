// Package fakeregistry is a tiny in-memory OCI registry for tests: enough of
// the distribution API for oras push and pull (/v2/, manifests by tag or
// digest, monolithic blob upload, blob GET/HEAD), an optional Basic
// challenge, and request counters.
package fakeregistry

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Registry is the fake. Create it with New.
type Registry struct {
	*httptest.Server

	mu        sync.Mutex
	blobs     map[string][]byte // digest -> bytes
	manifests map[string]manifest
	requests  []string
	user, pw  string
	failWith  int
}

type manifest struct {
	mediaType string
	body      []byte
}

// New starts a registry. A non-empty user demands HTTP Basic credentials.
func New(user, password string) *Registry {
	r := &Registry{blobs: map[string][]byte{}, manifests: map[string]manifest{}, user: user, pw: password}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))

	return r
}

// Host is host:port, usable as the registry part of a reference.
func (r *Registry) Host() string { return strings.TrimPrefix(r.URL, "http://") }

// Digest returns the sha256 digest string of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PutBlob stores a blob and returns its digest.
func (r *Registry) PutBlob(data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	d := Digest(data)
	r.blobs[d] = data

	return d
}

// PutManifest stores a manifest under name:tag and by digest, returning the digest.
func (r *Registry) PutManifest(name, tag, mediaType string, body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	d := Digest(body)
	m := manifest{mediaType: mediaType, body: body}
	r.manifests[name+":"+d] = m
	r.manifests[name+":"+tag] = m

	return d
}

// Corrupt replaces the bytes stored under a blob digest, so the registry
// serves content that no longer matches it.
func (r *Registry) Corrupt(digest string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.blobs[digest] = data
}

// SetCredentials changes the Basic credentials the registry demands.
func (r *Registry) SetCredentials(user, password string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.user, r.pw = user, password
}

// FailWith makes every manifest request answer with status (0 stops).
func (r *Registry) FailWith(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.failWith = status
}

// Count returns how many requests so far had a "METHOD path" containing substr.
func (r *Registry) Count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0

	for _, line := range r.requests {
		if strings.Contains(line, substr) {
			n++
		}
	}

	return n
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	r.mu.Unlock()

	r.mu.Lock()
	user, pw := r.user, r.pw
	r.mu.Unlock()

	if user != "" {
		if u, p, ok := req.BasicAuth(); !ok || u != user || p != pw {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
	}

	path := strings.TrimPrefix(req.URL.Path, "/v2/")

	switch {
	case path == "":
		w.WriteHeader(http.StatusOK)
	case strings.Contains(path, "/manifests/"):
		name, ref, _ := strings.Cut(path, "/manifests/")
		r.manifest(w, req, name, ref)
	case strings.HasSuffix(path, "/blobs/uploads/"):
		name := strings.TrimSuffix(path, "/blobs/uploads/")
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/session")
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(path, "/blobs/uploads/"):
		r.upload(w, req)
	case strings.Contains(path, "/blobs/"):
		_, d, _ := strings.Cut(path, "/blobs/")
		r.blob(w, req, d)
	default:
		http.NotFound(w, req)
	}
}

func (r *Registry) manifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.failWith != 0 {
		w.WriteHeader(r.failWith)
		return
	}

	if req.Method == http.MethodPut {
		body, _ := io.ReadAll(req.Body)
		d := Digest(body)
		m := manifest{mediaType: req.Header.Get("Content-Type"), body: body}
		r.manifests[name+":"+d] = m
		r.manifests[name+":"+ref] = m
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)

		return
	}

	m, ok := r.manifests[name+":"+ref]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"unknown"}]}`)

		return
	}

	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", Digest(m.body))
	http.ServeContent(w, req, "", timeZero, strings.NewReader(string(m.body)))
}

func (r *Registry) upload(w http.ResponseWriter, req *http.Request) {
	d := req.URL.Query().Get("digest")
	body, _ := io.ReadAll(req.Body)

	if req.Method != http.MethodPut || d != Digest(body) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	r.blobs[d] = body
	r.mu.Unlock()
	w.Header().Set("Docker-Content-Digest", d)
	w.WriteHeader(http.StatusCreated)
}

func (r *Registry) blob(w http.ResponseWriter, req *http.Request, d string) {
	r.mu.Lock()
	data, ok := r.blobs[d]
	r.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Docker-Content-Digest", d)
	http.ServeContent(w, req, "", timeZero, strings.NewReader(string(data)))
}

var timeZero time.Time
