package smserver

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

// Stats is a snapshot of the server's counters.
type Stats struct {
	CacheHits     int64 // served from the unpack cache
	Loads         int64 // registry pulls started
	LoadFailures  int64 // pulls that ended in 502
	Refusals      int64 // artifacts refused
	Denied        int64 // registry answered 401/403
	NotFound      int64 // tags absent from the registry
	NegativeHits  int64 // requests answered from the negative cache
	Evictions     int64 // (app, release) entries evicted
	CacheBytes    int64
	NegativeItems int
}

type metrics struct {
	hits, loads, failures, refusals, denied, notFound, negHits, evictions atomic.Int64

	mu       sync.Mutex
	requests map[int]int64
}

func (m *metrics) request(code int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.requests == nil {
		m.requests = map[int]int64{}
	}

	m.requests[code]++
}

// write emits the Prometheus text exposition format.
func (m *metrics) write(w io.Writer, s Stats) {
	counter := func(name, help string, v int64) {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counter("smctl_cache_hits_total", "Requests served from the unpack cache.", s.CacheHits)
	counter("smctl_loads_total", "Registry pulls started.", s.Loads)
	counter("smctl_load_failures_total", "Registry pulls that failed with 502.", s.LoadFailures)
	counter("smctl_refusals_total", "Artifacts refused as malformed or oversized.", s.Refusals)
	counter("smctl_denied_total", "Pulls the registry denied (401/403); answered 404.", s.Denied)
	counter("smctl_not_found_total", "Releases absent from the registry.", s.NotFound)
	counter("smctl_negative_hits_total", "Requests answered from the negative cache.", s.NegativeHits)
	counter("smctl_evictions_total", "Unpacked releases evicted from the cache.", s.Evictions)
	_, _ = fmt.Fprintf(w, "# HELP smctl_cache_bytes Unpacked bytes held.\n# TYPE smctl_cache_bytes gauge\nsmctl_cache_bytes %d\n", s.CacheBytes)
	_, _ = fmt.Fprintf(w, "# HELP smctl_negative_entries Negative cache entries.\n# TYPE smctl_negative_entries gauge\n")
	_, _ = fmt.Fprintf(w, "smctl_negative_entries %d\n", s.NegativeItems)

	m.mu.Lock()
	defer m.mu.Unlock()

	codes := make([]int, 0, len(m.requests))
	for c := range m.requests {
		codes = append(codes, c)
	}

	sort.Ints(codes)
	_, _ = fmt.Fprintf(w, "# HELP smctl_requests_total Source-map requests by status.\n# TYPE smctl_requests_total counter\n")

	for _, c := range codes {
		_, _ = fmt.Fprintf(w, "smctl_requests_total{code=\"%d\"} %d\n", c, m.requests[c])
	}
}
