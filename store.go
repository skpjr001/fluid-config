package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type snapshot struct {
	apis   map[string]*APISpec // "v1/holidays" -> spec
	byFile map[string]*APISpec
}

// Store keeps an immutable snapshot that readers load lock-free.
// Writes and reloads are serialized by mu and swap in a new snapshot.
type Store struct {
	dir      string
	snap     atomic.Pointer[snapshot]
	mu       sync.Mutex
	lastHash string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	s.snap.Store(&snapshot{apis: map[string]*APISpec{}, byFile: map[string]*APISpec{}})
	if err := s.Reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Get(key string) *APISpec { return s.snap.Load().apis[key] }

func (s *Store) All() []*APISpec {
	m := s.snap.Load().apis
	out := make([]*APISpec, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// listFiles walks apis_dir recursively (sub-folders allowed for grouping),
// skipping hidden files/dirs such as our own .tmp-* files.
func (s *Store) listFiles() ([]string, error) {
	var files []string
	err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != s.dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".json") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (s *Store) fingerprint() (string, []string, error) {
	files, err := s.listFiles()
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			fmt.Fprintf(h, "%s|%d|%d\n", f, st.Size(), st.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

func parseSpecFile(path string) (*APISpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep numbers exact (large ids, amounts)
	dec.DisallowUnknownFields()
	var spec APISpec
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	spec.file = path
	if err := spec.normalize(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Reload re-parses every file when anything changed. A broken file is
// logged and its last good version stays live.
func (s *Store) Reload(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, files, err := s.fingerprint()
	if err != nil {
		return err
	}
	if !force && fp == s.lastHash {
		return nil
	}
	old := s.snap.Load()
	next := &snapshot{apis: map[string]*APISpec{}, byFile: map[string]*APISpec{}}
	for _, f := range files {
		spec, err := parseSpecFile(f)
		if err != nil {
			slog.Error("invalid api file, keeping previous version", "file", f, "err", err)
			spec = old.byFile[f]
			if spec == nil {
				continue
			}
		}
		if other, dup := next.apis[spec.key()]; dup {
			slog.Error("duplicate api, skipped", "key", spec.key(), "file", f, "already_in", other.file)
			continue
		}
		next.apis[spec.key()] = spec
		next.byFile[f] = spec
	}
	s.snap.Store(next)
	s.lastHash = fp
	keys := make([]string, 0, len(next.apis))
	for k := range next.apis {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	slog.Info("apis loaded", "count", len(keys), "apis", strings.Join(keys, ","))
	return nil
}

func (s *Store) Watch(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := s.Reload(false); err != nil {
				slog.Error("reload failed", "err", err)
			}
		}
	}
}

// Mutate runs fn against the latest data under the write lock, persists
// the result to the API's own file atomically, then swaps the snapshot.
// fn must return a NEW slice and never modify rows in place.
func (s *Store) Mutate(key string, fn func(spec *APISpec) ([]map[string]any, error)) (*APISpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.snap.Load()
	spec := cur.apis[key]
	if spec == nil {
		return nil, &httpErr{404, "api not found"}
	}
	data, err := fn(spec)
	if err != nil {
		return nil, err
	}
	n, err := spec.withData(data)
	if err != nil {
		return nil, &httpErr{409, err.Error()}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := writeAtomic(n.file, buf.Bytes()); err != nil {
		return nil, err
	}
	next := &snapshot{apis: make(map[string]*APISpec, len(cur.apis)), byFile: make(map[string]*APISpec, len(cur.byFile))}
	for k, v := range cur.apis {
		next.apis[k] = v
	}
	for k, v := range cur.byFile {
		next.byFile[k] = v
	}
	next.apis[key], next.byFile[n.file] = n, n
	s.snap.Store(next)
	// lastHash is left stale on purpose: the next poll re-parses (same content),
	// so a hand edit to another file in the same window is never swallowed.
	return n, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		os.Chmod(tmp.Name(), st.Mode().Perm())
	}
	return os.Rename(tmp.Name(), path)
}
