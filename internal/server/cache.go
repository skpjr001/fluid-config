package server

import (
	"container/list"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ResponseCache keeps rendered GET responses in memory, per API, for APIs
// whose file sets "cache": {"enabled": true}. An entry is keyed by the data
// version (ETag) and the normalized query, so any change to the data (a
// write, or an edit of the file) makes older entries unreachable at once;
// they age out by TTL or LRU. Purge empties one API's cache on demand.
type ResponseCache struct {
	mu   sync.Mutex
	apis map[string]*apiCache
}

type apiCache struct {
	lru     *list.List // front = most recent
	entries map[string]*list.Element
	bytes   int64
	hits    atomic.Int64
	misses  atomic.Int64
	purged  time.Time
}

type cachedResponse struct {
	key     string
	status  int
	header  http.Header
	body    []byte
	expires time.Time
}

// CacheStats is what the console shows for one API.
type CacheStats struct {
	Enabled    bool      `json:"enabled"`
	TTLSeconds int       `json:"ttl_seconds,omitempty"`
	MaxEntries int       `json:"max_entries,omitempty"`
	Entries    int       `json:"entries"`
	Bytes      int64     `json:"bytes"`
	Hits       int64     `json:"hits"`
	Misses     int64     `json:"misses"`
	PurgedAt   time.Time `json:"purged_at,omitzero"`
}

func NewResponseCache() *ResponseCache { return &ResponseCache{apis: map[string]*apiCache{}} }

func (c *ResponseCache) api(key string) *apiCache {
	a := c.apis[key]
	if a == nil {
		a = &apiCache{lru: list.New(), entries: map[string]*list.Element{}}
		c.apis[key] = a
	}
	return a
}

// Get returns a fresh entry or nil.
func (c *ResponseCache) Get(api, key string) *cachedResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.api(api)
	el := a.entries[key]
	if el == nil {
		a.misses.Add(1)
		return nil
	}
	e := el.Value.(*cachedResponse)
	if time.Now().After(e.expires) {
		a.remove(el)
		a.misses.Add(1)
		return nil
	}
	a.lru.MoveToFront(el)
	a.hits.Add(1)
	return e
}

func (a *apiCache) remove(el *list.Element) {
	e := el.Value.(*cachedResponse)
	a.bytes -= int64(len(e.body))
	delete(a.entries, e.key)
	a.lru.Remove(el)
}

// maxCachedBody keeps one huge response from filling memory.
const maxCachedBody = 8 << 20

// Put stores a response, evicting the least recently used entries beyond max.
func (c *ResponseCache) Put(api string, spec *CacheSpec, e *cachedResponse) {
	if len(e.body) > maxCachedBody {
		return
	}
	e.expires = time.Now().Add(time.Duration(spec.TTLSeconds) * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.api(api)
	if el := a.entries[e.key]; el != nil {
		a.remove(el)
	}
	a.entries[e.key] = a.lru.PushFront(e)
	a.bytes += int64(len(e.body))
	for a.lru.Len() > spec.MaxEntries {
		a.remove(a.lru.Back())
	}
}

// Purge drops every entry of one API on request (the console's Purge
// cache button); it returns how many were dropped.
func (c *ResponseCache) Purge(api string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.api(api)
	a.purged = time.Now().UTC()
	return a.clear()
}

// Invalidate drops an API's entries after its data changed. Entries are
// keyed by the data version and could never be served again; this frees
// their memory at once.
func (c *ResponseCache) Invalidate(api string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.apis[api]; a != nil {
		a.clear()
	}
}

func (a *apiCache) clear() int {
	n := a.lru.Len()
	a.lru.Init()
	a.entries = map[string]*list.Element{}
	a.bytes = 0
	return n
}

// Forget removes an API that no longer exists or no longer caches.
func (c *ResponseCache) Forget(api string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.apis, api)
}

func (c *ResponseCache) Stats(spec *APISpec) CacheStats {
	st := CacheStats{Enabled: spec.cacheOn()}
	if spec.Cache != nil {
		st.TTLSeconds, st.MaxEntries = spec.Cache.TTLSeconds, spec.Cache.MaxEntries
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.apis[spec.key()]; a != nil {
		st.Entries, st.Bytes, st.PurgedAt = a.lru.Len(), a.bytes, a.purged
		st.Hits, st.Misses = a.hits.Load(), a.misses.Load()
	}
	return st
}

// TotalBytes is the memory held by all cached bodies (for /metrics).
func (c *ResponseCache) TotalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, a := range c.apis {
		n += a.bytes
	}
	return n
}

// bufferWriter records a response so it can be cached and replayed.
type bufferWriter struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (b *bufferWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
	b.ResponseWriter.WriteHeader(code)
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if len(b.body) <= maxCachedBody {
		b.body = append(b.body, p...)
	}
	return b.ResponseWriter.Write(p)
}

func (b *bufferWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }
