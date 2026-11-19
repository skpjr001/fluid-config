package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Layout of the dynamic directory (hot-reloaded, like Traefik's file provider):
//
//	<dynamic_dir>/apis/**/*.json   one API per file (sub-folders are for grouping)
//	<dynamic_dir>/admin.json       console tokens (viewer / editor)
//	<dynamic_dir>/access.json      console and catalog IP allowlists
const apisSubdir = "apis"

// deletedDir keeps API files deleted from the console (hidden: never loaded).
const deletedDir = ".deleted"

type snapshot struct {
	apis     map[string]*APISpec // "v1/holidays" -> spec
	byFile   map[string]*APISpec // path inside apis/ -> spec
	errors   map[string]string   // file (relative to dynamic_dir) -> problem
	admin    *AdminConfig
	access   *AccessConfig
	loadedAt time.Time
}

func emptySnapshot() *snapshot {
	return &snapshot{apis: map[string]*APISpec{}, byFile: map[string]*APISpec{}, errors: map[string]string{},
		admin: &AdminConfig{}, access: emptyAccess(), loadedAt: time.Now()}
}

// Store keeps an immutable snapshot that readers load lock-free. Writes and
// reloads are serialized by mu and swap in a new snapshot.
type Store struct {
	dir      string
	snap     atomic.Pointer[snapshot]
	mu       sync.Mutex
	lastHash string

	// onReload runs after every reload that found changes, with the audit
	// entries for API files edited by hand (nil on the first load).
	onReload func(ok bool, changes []AuditEntry)
}

func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	s.snap.Store(emptySnapshot())
	os.MkdirAll(filepath.Join(dir, apisSubdir), 0o750) // best effort: bootstrap reports problems
	if err := s.Reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) apisDir() string                     { return filepath.Join(s.dir, apisSubdir) }
func (s *Store) Get(key string) *APISpec             { return s.snap.Load().apis[key] }
func (s *Store) Admin() *AdminConfig                 { return s.snap.Load().admin }
func (s *Store) Access() *AccessConfig               { return s.snap.Load().access }
func (s *Store) Errors() map[string]string           { return s.snap.Load().errors }
func (s *Store) LoadedAt() time.Time                 { return s.snap.Load().loadedAt }
func (s *Store) All() []*APISpec                     { return s.snap.Load().all() }
func (s *Store) OnReload(f func(bool, []AuditEntry)) { s.onReload = f }

func (sn *snapshot) all() []*APISpec {
	out := make([]*APISpec, 0, len(sn.apis))
	for _, k := range slices.Sorted(maps.Keys(sn.apis)) {
		out = append(out, sn.apis[k])
	}
	return out
}

// listAPIFiles walks apis/ recursively, skipping hidden files and folders
// (our .tmp-* files and .deleted/).
func listAPIFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
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
	files, err := listAPIFiles(s.apisDir())
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	for _, f := range append(slices.Clone(files), filepath.Join(s.dir, "admin.json"), filepath.Join(s.dir, "access.json")) {
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
	spec, err := decodeSpec(b)
	if err != nil {
		return nil, err
	}
	if err := spec.normalize(); err != nil {
		return nil, err
	}
	spec.file = path
	if st, err := os.Stat(path); err == nil {
		spec.modTime = st.ModTime()
	}
	return spec, nil
}

func readStrict(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(bytes.NewReader(b), v)
}

// loadDir parses everything in a dynamic dir. A broken file is reported in
// errors and its last good version (from old) stays live.
func loadDir(dir string, old *snapshot) *snapshot {
	if old == nil {
		old = emptySnapshot()
	}
	next := emptySnapshot()
	fail := func(rel string, err error) { next.errors[filepath.ToSlash(rel)] = err.Error() }

	files, err := listAPIFiles(filepath.Join(dir, apisSubdir))
	if err != nil {
		fail(apisSubdir, err)
	}
	for _, f := range files {
		rel, _ := filepath.Rel(filepath.Join(dir, apisSubdir), f)
		rel = filepath.ToSlash(rel)
		spec, err := parseSpecFile(f)
		if err != nil {
			fail(apisSubdir+"/"+rel, err)
			if spec = old.byFile[rel]; spec == nil {
				continue
			}
		}
		spec.rel = rel
		if other, dup := next.apis[spec.key()]; dup {
			fail(apisSubdir+"/"+rel, fmt.Errorf("duplicate api %s (already defined in %s/%s); this file is ignored", spec.key(), apisSubdir, other.rel))
			continue
		}
		next.apis[spec.key()] = spec
		next.byFile[rel] = spec
	}

	next.admin = old.admin
	var ac AdminConfig
	switch err := readStrict(filepath.Join(dir, "admin.json"), &ac); {
	case errors.Is(err, fs.ErrNotExist):
		next.admin = &AdminConfig{}
	case err == nil:
		if err := ac.normalize(); err != nil {
			fail("admin.json", err)
		} else {
			next.admin = &ac
		}
	default:
		fail("admin.json", err)
	}

	next.access = old.access
	var acc AccessConfig
	switch err := readStrict(filepath.Join(dir, "access.json"), &acc); {
	case errors.Is(err, fs.ErrNotExist):
		next.access = emptyAccess()
	case err == nil:
		if err := acc.normalize(); err != nil {
			fail("access.json", err)
		} else {
			next.access = &acc
		}
	default:
		fail("access.json", err)
	}
	return next
}

// Reload re-parses every file when anything changed.
func (s *Store) Reload(force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(force)
}

func (s *Store) reloadLocked(force bool) error {
	fp, _, err := s.fingerprint()
	if err != nil {
		return err
	}
	if !force && fp == s.lastHash {
		return nil
	}
	old := s.snap.Load()
	first := s.lastHash == ""
	next := loadDir(s.dir, old)
	s.snap.Store(next)
	s.lastHash = fp

	var changes []AuditEntry
	if !first {
		changes = fileChanges(old, next)
	}
	for f, e := range next.errors {
		if old.errors[f] != e {
			slog.Error("invalid config file, keeping its previous version", "kind", "config", "file", f, "err", e)
		}
	}
	keys := slices.Sorted(maps.Keys(next.apis))
	if first || !slices.Equal(keys, slices.Sorted(maps.Keys(old.apis))) || len(changes) > 0 {
		slog.Info("apis loaded", "kind", "config", "count", len(keys), "apis", strings.Join(keys, ","))
	}
	if s.onReload != nil {
		s.onReload(len(next.errors) == 0, changes)
	}
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
				slog.Error("reload failed", "kind", "config", "err", err)
			}
		}
	}
}

// ---------- writes ----------

// ErrInvalid marks validation errors (HTTP 400/422) as opposed to I/O errors.
var ErrInvalid = errors.New("invalid")

type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

// writeErr explains a failed write of an API file.
func writeErr(rel string, err error) error {
	if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
		return &httpErr{409, "the file " + apisSubdir + "/" + rel + " is read-only here; change it in the file itself (" + err.Error() + ")"}
	}
	return err
}

// swap replaces one API in the snapshot (copy-on-write).
func (s *Store) swap(oldKey string, n *APISpec) {
	cur := s.snap.Load()
	next := *cur
	next.apis = maps.Clone(cur.apis)
	next.byFile = maps.Clone(cur.byFile)
	if old := cur.apis[oldKey]; old != nil {
		delete(next.apis, oldKey)
		delete(next.byFile, old.rel)
	}
	if n != nil {
		next.apis[n.key()] = n
		next.byFile[n.rel] = n
	}
	s.snap.Store(&next)
	// lastHash is left stale on purpose: the next poll re-parses (same
	// content), so a hand edit to another file in the same window is never lost.
}

// fresh returns the API, first re-reading its file if it changed on disk
// since it was loaded, so a write never overwrites a hand edit.
func (s *Store) fresh(key string) (*APISpec, error) {
	spec := s.snap.Load().apis[key]
	if spec == nil {
		return nil, &httpErr{404, "api not found"}
	}
	if st, err := os.Stat(spec.file); err == nil && !st.ModTime().Equal(spec.modTime) {
		if err := s.reloadLocked(true); err != nil {
			return nil, err
		}
		if spec = s.snap.Load().apis[key]; spec == nil {
			return nil, &httpErr{404, "api not found (its file changed)"}
		}
		if e := s.snap.Load().errors[apisSubdir+"/"+spec.rel]; e != "" {
			return nil, &httpErr{409, "the file was edited by hand and is now invalid; fix it first: " + e}
		}
	}
	return spec, nil
}

func (s *Store) persist(n *APISpec) error {
	b, err := n.encode()
	if err != nil {
		return err
	}
	if err := writeAtomicMode(n.file, b, filePerm(n.file, 0o640)); err != nil {
		return writeErr(n.rel, err)
	}
	if st, err := os.Stat(n.file); err == nil {
		n.modTime = st.ModTime()
	}
	return nil
}

// Mutate runs fn against the latest data under the write lock, persists the
// result to the API's own file atomically, then swaps the snapshot.
// fn must return a NEW slice and never modify rows in place.
func (s *Store) Mutate(key string, fn func(spec *APISpec) ([]map[string]any, error)) (*APISpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, err := s.fresh(key)
	if err != nil {
		return nil, err
	}
	data, err := fn(spec)
	if err != nil {
		return nil, err
	}
	n, err := spec.withData(data)
	if err != nil {
		return nil, &httpErr{409, err.Error()}
	}
	if err := s.persist(n); err != nil {
		return nil, err
	}
	s.swap(key, n)
	return n, nil
}

// SaveContract replaces an API's contract (everything but its data). The
// existing records must still be valid under the new contract.
func (s *Store) SaveContract(key string, c *APISpec) (*APISpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.fresh(key)
	if err != nil {
		return nil, err
	}
	n := c.clone()
	n.Data = cur.clone().Data
	n.file, n.rel = cur.file, cur.rel
	if err := n.normalize(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if n.key() != key {
		if other := s.snap.Load().apis[n.key()]; other != nil {
			return nil, &httpErr{409, "api " + n.key() + " already exists in " + other.rel}
		}
	}
	if err := s.persist(n); err != nil {
		return nil, err
	}
	s.swap(key, n)
	return n, nil
}

var relRe = `^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*\.json$`

// Create writes a new API file. rel is its path inside apis/ ("" = <name>.json).
func (s *Store) Create(spec *APISpec, rel string) (*APISpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := spec.clone()
	if err := n.normalize(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if other := s.snap.Load().apis[n.key()]; other != nil {
		return nil, &httpErr{409, "api " + n.key() + " already exists in " + other.rel}
	}
	if rel == "" {
		rel = n.Name + ".json"
		if _, err := os.Stat(filepath.Join(s.apisDir(), rel)); err == nil {
			rel = n.Name + "." + n.Version + ".json"
		}
	}
	if ok, _ := regexpMatch(relRe, rel); !ok || strings.Contains(rel, "..") {
		return nil, fmt.Errorf("%w: file must be a lowercase path ending in .json, like market/holidays.json", ErrInvalid)
	}
	n.rel, n.file = rel, filepath.Join(s.apisDir(), filepath.FromSlash(rel))
	if _, err := os.Stat(n.file); err == nil {
		return nil, &httpErr{409, apisSubdir + "/" + rel + " already exists"}
	}
	if err := os.MkdirAll(filepath.Dir(n.file), 0o750); err != nil {
		return nil, writeErr(rel, err)
	}
	if n.Schema == "" {
		n.Schema = SchemaBase + "api.schema.json"
	}
	if err := s.persist(n); err != nil {
		return nil, err
	}
	s.swap("", n)
	return n, nil
}

// Delete moves an API file to apis/.deleted/ (never loaded, easy to restore).
func (s *Store) Delete(key string) (*APISpec, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec := s.snap.Load().apis[key]
	if spec == nil {
		return nil, "", &httpErr{404, "api not found"}
	}
	dst := filepath.Join(s.apisDir(), deletedDir, strings.ReplaceAll(spec.rel, "/", "__")+"."+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return nil, "", writeErr(spec.rel, err)
	}
	if err := os.Rename(spec.file, dst); err != nil {
		return nil, "", writeErr(spec.rel, err)
	}
	s.swap(key, nil)
	rel, _ := filepath.Rel(s.dir, dst)
	return spec, filepath.ToSlash(rel), nil
}

// SaveAccess writes access.json.
func (s *Store) SaveAccess(a *AccessConfig) error {
	if err := a.normalize(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	a.Schema = SchemaBase + "access.schema.json"
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "access.json")
	if err := writeAtomicMode(path, b, filePerm(path, 0o640)); err != nil {
		return err
	}
	return s.reloadLocked(true)
}

func regexpMatch(re, s string) (bool, error) { return regexp.MatchString(re, s) }

func filePerm(path string, def os.FileMode) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return def
}

func writeAtomicMode(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
