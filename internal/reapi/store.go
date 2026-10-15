package reapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu  sync.Mutex
	dir string
	max int64
}

func Open(dir string, max int64) (*Store, error) {
	if max <= 0 {
		return nil, fmt.Errorf("cache max size must be positive")
	}
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, max: max}
	return s, s.evict()
}
func digest(b []byte) []byte {
	h := sha256.Sum256(b)
	return (Digest{Hash: hex.EncodeToString(h[:]), Size: int64(len(b))}).marshal()
}
func key(b []byte) (string, error) { d, e := parseDigest(b); return d.Hash, e }

func (s *Store) read(kind string, d []byte) ([]byte, error) {
	if kind == "cas-" && isEmptyDigest(d) {
		return []byte{}, nil
	}
	k, e := key(d)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, kind+k)
	b, e := os.ReadFile(p)
	if e == nil {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}

	if e == nil && kind == "cas-" && uint64(len(b)) != get(d, 2).v {
		return nil, os.ErrNotExist
	}
	return b, e
}
func (s *Store) put(kind string, d, b []byte) error {
	k, e := key(d)
	if e != nil {
		return e
	}
	if kind == "cas-" && (string(get(digest(b), 1).b) != k || uint64(len(b)) != get(d, 2).v) {
		return fmt.Errorf("digest mismatch")
	}
	if int64(len(b)) > s.max {
		return fmt.Errorf("blob exceeds cache size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := os.CreateTemp(s.dir, ".write-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(s.dir, kind+k)); e != nil {
		return e
	}
	return s.evict()
}
func (s *Store) evict() error {
	es, e := os.ReadDir(s.dir)
	if e != nil {
		return e
	}
	type entry struct {
		p string
		n int64
		t time.Time
	}
	var entries []entry
	var total int64
	for _, x := range es {
		if x.IsDir() || !cacheFile(x.Name()) {
			continue
		}
		i, e := x.Info()
		if e != nil {
			return e
		}
		entries = append(entries, entry{x.Name(), i.Size(), i.ModTime()})
		total += i.Size()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
	for _, x := range entries {
		if total <= s.max {
			break
		}
		if e = os.Remove(filepath.Join(s.dir, x.p)); e != nil {
			return e
		}
		total -= x.n
	}
	return nil
}

func (s *Store) uploadPath(name string) string {
	h := sha256.Sum256([]byte(name))
	return filepath.Join(s.dir, "partial-"+hex.EncodeToString(h[:]))
}

// Partial uploads share the disk budget with committed blobs. Eviction may discard
// an interrupted upload; clients can restart it after QueryWriteStatus returns NOT_FOUND.
func (s *Store) appendUpload(name string, d []byte, offset uint64, chunk []byte, finish bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.uploadPath(name)
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return 0, e
	}
	if uint64(info.Size()) != offset {
		return 0, fmt.Errorf("write offset mismatch")
	}
	size := info.Size() + int64(len(chunk))
	if size > s.max || uint64(size) > get(d, 2).v {
		return 0, fmt.Errorf("upload exceeds size limit")
	}
	if _, e = f.Seek(0, 2); e != nil {
		return 0, e
	}
	if _, e = f.Write(chunk); e != nil {
		return 0, e
	}
	if finish {
		if _, e = f.Seek(0, 0); e != nil {
			return 0, e
		}
		h := sha256.New()
		if _, e = io.Copy(h, f); e != nil {
			return 0, e
		}
		k, e := key(d)
		if e != nil {
			return 0, e
		}
		if hex.EncodeToString(h.Sum(nil)) != k || uint64(size) != get(d, 2).v {
			os.Remove(p)
			return 0, fmt.Errorf("digest mismatch")
		}
		if e = os.Rename(p, filepath.Join(s.dir, "cas-"+k)); e != nil {
			return 0, e
		}
	}
	return size, s.evict()
}

// Open files pin bytes while LRU eviction unlinks their names.
func (s *Store) openBlob(d []byte) (*os.File, error) {
	k, e := key(d)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.dir, "cas-"+k)
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil || uint64(info.Size()) != get(d, 2).v {
		f.Close()
		return nil, os.ErrNotExist
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return f, nil
}

func (s *Store) has(d []byte) bool {
	if isEmptyDigest(d) {
		return true
	}
	f, e := s.openBlob(d)
	if e != nil {
		return false
	}
	f.Close()
	return true
}

func isEmptyDigest(b []byte) bool {
	d, e := parseDigest(b)
	return e == nil && d.Size == 0 && d.Hash == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}

func cacheFile(name string) bool {
	if strings.HasPrefix(name, ".write-") {
		return true
	}
	for _, prefix := range []string{"cas-", "ac-", "partial-"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			raw, e := hex.DecodeString(rest)
			return e == nil && len(raw) == 32 && hex.EncodeToString(raw) == rest
		}
	}
	return false
}
