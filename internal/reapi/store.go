package reapi

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var errCapacity = errors.New("blob exceeds cache size or declared size")

var errOffset = errors.New("write offset mismatch")

// mu protects only the index. File operations use bounded per-name locks;
// eviction never holds mu while waiting for disk or another writer.
type Store struct {
	mu       sync.Mutex
	files    map[string]*list.Element
	lru      list.List
	total    int64
	locks    [256]sync.Mutex
	eviction sync.Mutex
	dir      string
	max      int64
}
type cacheEntry struct {
	name  string
	size  int64
	atime time.Time
	hash  hash.Hash // partial uploads only; nil after restart until first resume
}

func (s *Store) fileLock(name string) *sync.Mutex {
	h := sha256.Sum256([]byte(name))
	return &s.locks[h[0]]
}
func (s *Store) record(name string, size int64, h hash.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(name)
	s.files[name] = s.lru.PushBack(&cacheEntry{name, size, time.Now(), h})
	s.total += size
}
func (s *Store) forgetLocked(name string) {
	if el := s.files[name]; el != nil {
		s.total -= el.Value.(*cacheEntry).size
		s.lru.Remove(el)
		delete(s.files, name)
	}
}
func (s *Store) forget(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(name)
}
func (s *Store) touch(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el := s.files[name]; el != nil {
		el.Value.(*cacheEntry).atime = time.Now()
		s.lru.MoveToBack(el)
	}
}
func Open(dir string, max int64) (*Store, error) {
	if max <= 0 {
		return nil, fmt.Errorf("cache max size must be positive")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, max: max, files: make(map[string]*list.Element)}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []*cacheEntry
	for _, x := range es {
		if x.IsDir() || !cacheFile(x.Name()) {
			continue
		}
		info, err := x.Info()
		if err != nil {
			log.Printf("cache stat %s: %v", x.Name(), err)
			continue
		}
		entries = append(entries, &cacheEntry{name: x.Name(), size: info.Size(), atime: info.ModTime()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].atime.Before(entries[j].atime) })
	for _, x := range entries {
		s.files[x.name] = s.lru.PushBack(x)
		s.total += x.size
	}
	s.evict()
	return s, nil
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
	lock := s.fileLock(kind + k)
	lock.Lock()
	defer lock.Unlock()
	p := filepath.Join(s.dir, kind+k)
	if kind == "cas-" {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if uint64(info.Size()) != get(d, 2).v {
			return nil, os.ErrNotExist
		}
	}
	b, e := os.ReadFile(p)
	if e == nil {
		s.touch(kind + k)
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}

	if e == nil && kind == "cas-" && (uint64(len(b)) != get(d, 2).v || string(get(digest(b), 1).b) != k) {
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
		return errCapacity
	}
	defer s.evict()
	lock := s.fileLock(kind + k)
	lock.Lock()
	defer lock.Unlock()
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
	s.record(kind+k, int64(len(b)), nil)
	return nil
}

// A failed deletion stays indexed, but cannot prevent other victims being tried.
// Retry it on the next eviction pass; committed writes never fail due to eviction.
func (s *Store) evict() {
	s.mu.Lock()
	over := s.total > s.max
	s.mu.Unlock()
	if !over {
		return
	}
	s.eviction.Lock()
	defer s.eviction.Unlock()
	s.mu.Lock()
	remaining := s.lru.Len()
	s.mu.Unlock()
	for remaining > 0 {
		remaining--
		s.mu.Lock()
		if s.total <= s.max || s.lru.Len() == 0 {
			s.mu.Unlock()
			return
		}
		el := s.lru.Front()
		name := el.Value.(*cacheEntry).name
		s.mu.Unlock()
		lock := s.fileLock(name)
		lock.Lock()
		s.mu.Lock()
		// A concurrent access may have refreshed or replaced the candidate.
		if s.files[name] != el || s.lru.Front() != el {
			s.mu.Unlock()
			lock.Unlock()
			continue
		}
		s.mu.Unlock()
		err := os.Remove(filepath.Join(s.dir, name))
		if err == nil || os.IsNotExist(err) {
			s.forget(name)
		} else {
			log.Printf("cache evict %s: %v", name, err)
			s.touch(name)
		}
		lock.Unlock()
	}
}

func (s *Store) uploadPath(name string) string {
	h := sha256.Sum256([]byte(name))
	return filepath.Join(s.dir, "partial-"+hex.EncodeToString(h[:]))
}

// Partial uploads share the disk budget with committed blobs. Eviction may discard
// an interrupted upload; clients can restart it after QueryWriteStatus returns NOT_FOUND.
func (s *Store) appendUpload(name string, d []byte, offset uint64, chunk []byte, finish bool) (int64, error) {
	p := s.uploadPath(name)
	partial := filepath.Base(p)
	k, e := key(d)
	if e != nil {
		return 0, e
	}
	// Lock partial and destination in stripe order so commit is atomic with
	// respect to eviction and other writes, without holding the index mutex.
	defer s.evict()
	ph := sha256.Sum256([]byte(partial))
	dh := sha256.Sum256([]byte("cas-" + k))
	first, second := ph[0], dh[0]
	if first > second {
		first, second = second, first
	}
	s.locks[first].Lock()
	defer s.locks[first].Unlock()
	if first != second {
		s.locks[second].Lock()
		defer s.locks[second].Unlock()
	}
	flags := os.O_RDWR
	if offset == 0 {
		flags |= os.O_CREATE
	}
	f, e := os.OpenFile(p, flags, 0600)
	if os.IsNotExist(e) {
		return 0, errOffset
	}
	if e != nil {
		return 0, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return 0, e
	}
	if uint64(info.Size()) != offset {
		return 0, errOffset
	}
	size := info.Size() + int64(len(chunk))
	if size > s.max || uint64(size) > get(d, 2).v {
		os.Remove(p)
		s.forget(partial)
		return 0, errCapacity
	}
	s.mu.Lock()
	var h hash.Hash
	if el := s.files[partial]; el != nil {
		h = el.Value.(*cacheEntry).hash
	}
	s.mu.Unlock()
	if h == nil {
		h = sha256.New()
		if _, e = io.Copy(h, f); e != nil {
			return 0, e
		}
	}
	if _, e = f.Seek(0, io.SeekEnd); e != nil {
		return 0, e
	}
	written, e := f.Write(chunk)
	if e != nil {
		// A short write invalidates the hash; resume will reconstruct it.
		s.record(partial, info.Size()+int64(written), nil)
		return 0, e
	}
	h.Write(chunk)
	s.record(partial, size, h)
	if finish {
		if hex.EncodeToString(h.Sum(nil)) != k || uint64(size) != get(d, 2).v {
			os.Remove(p)
			s.forget(partial)
			return 0, fmt.Errorf("digest mismatch")
		}
		dest := "cas-" + k
		if e = os.Rename(p, filepath.Join(s.dir, dest)); e != nil {
			return 0, e
		}
		s.forget(partial)
		s.record(dest, size, nil)
	}
	return size, nil
}

// Open files pin bytes while LRU eviction unlinks their names.
func (s *Store) openBlob(d []byte) (*os.File, error) {
	k, e := key(d)
	if e != nil {
		return nil, e
	}
	lock := s.fileLock("cas-" + k)
	lock.Lock()
	defer lock.Unlock()
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
	s.touch("cas-" + k)
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

// copyInput verifies the content while copying, with bounded memory.
func (s *Store) copyInput(ctx context.Context, d []byte, dst io.Writer) error {
	if isEmptyDigest(d) {
		return ctx.Err()
	}
	f, err := s.openBlob(d)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), contextReader{ctx, f})
	if err != nil {
		return err
	}
	if uint64(n) != get(d, 2).v || hex.EncodeToString(h.Sum(nil)) != string(get(d, 1).b) {
		return os.ErrNotExist
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}
func (s *Store) putOutput(f *os.File) ([]byte, error) {
	tmp, err := os.CreateTemp(s.dir, ".write-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(f, MaxExecutorBlob+1))
	if err != nil {
		return nil, err
	}
	if n > MaxExecutorBlob || n > s.max {
		return nil, capacityError()
	}
	d := (Digest{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}).marshal()
	k, _ := key(d)
	lock := s.fileLock("cas-" + k)
	lock.Lock()
	err = os.Rename(tmp.Name(), filepath.Join(s.dir, "cas-"+k))
	if err == nil {
		s.record("cas-"+k, n, nil)
	}
	lock.Unlock()
	s.evict()
	return d, err
}
