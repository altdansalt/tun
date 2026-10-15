package reapi

// The input tree cache keeps large input directories that actions share (a toolchain such as
// the Go SDK, a built standard library) materialized once per executor data dir, and binds
// them read-only into each exec root instead of copying (and hashing) every file again: for a
// Go compile that was ~3s of each action (docs/bazel.md). A directory is cached the second
// time an action names it with the same digest, so only trees that are actually reused are
// kept; the topmost reused one is taken (a varying parent like external/ is not, its stable
// children are). Cached trees are never written after they are complete: actions see them
// read-only, and the files were verified against their digests while being copied in.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/workspace"
)

const (
	treeMinFiles  = 64      // a directory with fewer files (recursively) and
	treeMinBytes  = 4 << 20 // fewer bytes is copied, not cached
	treeMaxMounts = 32      // cached trees bound into one action at most
	treeCacheMax  = 2 << 30 // bytes of cached trees per data dir; least recently used go first
	treeSeenMax   = 100000  // directory digests remembered as seen once
)

type treeCache struct {
	dir     string
	mu      sync.Mutex
	entries map[string]*treeEntry // complete or being materialized, by digest key
	seen    map[string]bool       // large directories named once so far
	sizes   map[string]treeSize   // recursive sizes of large directories' digests
	total   int64                 // bytes of complete entries
	tick    uint64
}

type treeEntry struct {
	path  string
	size  int64
	ready chan struct{} // closed when materialized (err says whether it worked)
	err   error
	refs  int // actions using it: not evicted meanwhile
	used  uint64
}

type treeSize struct {
	files int
	bytes uint64
}

// treeCacheDir is in the data dir; SweepExecRoots ("action-*") deletes it at startup, so a
// half-written tree from a crash is never used.
const treeCacheDir = "action-trees"

var treeCaches sync.Map // data dir -> *treeCache

func treeCacheFor(dataDir string) *treeCache {
	c, _ := treeCaches.LoadOrStore(dataDir, &treeCache{dir: filepath.Join(dataDir, treeCacheDir),
		entries: map[string]*treeEntry{}, seen: map[string]bool{}, sizes: map[string]treeSize{}})
	return c.(*treeCache)
}

func treeKey(d []byte) string { return fmt.Sprintf("%s-%d", get(d, 1).b, get(d, 2).v) }

// size returns the number of files and their bytes under directory d.
func (c *treeCache) size(ctx context.Context, s BlobStore, d []byte, depth int) (treeSize, error) {
	k := treeKey(d)
	c.mu.Lock()
	sz, ok := c.sizes[k]
	c.mu.Unlock()
	if ok {
		return sz, nil
	}
	if depth > 256 {
		return sz, fmt.Errorf("input tree too deep")
	}
	if err := ctx.Err(); err != nil {
		return sz, err
	}
	b, err := inputRead(s, d)
	if err != nil {
		return sz, err
	}
	for _, f := range all(b, 1) {
		sz.files++
		sz.bytes += get(get(f, 2).b, 2).v
	}
	for _, sub := range all(b, 2) {
		ssz, err := c.size(ctx, s, get(sub, 2).b, depth+1)
		if err != nil {
			return sz, err
		}
		sz.files += ssz.files
		sz.bytes += ssz.bytes
	}
	if sz.files >= treeMinFiles || sz.bytes >= treeMinBytes {
		c.mu.Lock()
		if len(c.sizes) >= treeSeenMax {
			clear(c.sizes)
		}
		c.sizes[k] = sz
		c.mu.Unlock()
	}
	return sz, nil
}

// acquire returns the materialized tree of directory d, copying it in first if needed. The
// caller releases it once its action has ended.
func (c *treeCache) acquire(ctx context.Context, s BlobStore, d []byte) (*treeEntry, error) {
	k := treeKey(d)
	c.mu.Lock()
	e := c.entries[k]
	if e == nil {
		e = &treeEntry{ready: make(chan struct{})}
		c.entries[k] = e
		e.refs++
		c.mu.Unlock()
		e.path, e.size, e.err = c.fill(ctx, s, d, k)
		c.mu.Lock()
		if e.err != nil {
			delete(c.entries, k)
		} else {
			c.total += e.size
		}
		close(e.ready)
		c.mu.Unlock()
		if e.err != nil {
			return nil, e.err
		}
		c.evict()
		return e, nil
	}
	e.refs++
	c.mu.Unlock()
	select {
	case <-e.ready:
	case <-ctx.Done():
		c.release(e)
		return nil, ctx.Err()
	}
	if e.err != nil {
		return nil, e.err
	}
	return e, nil
}

// fill materializes directory d into a new dir of the cache and returns its path and size.
func (c *treeCache) fill(ctx context.Context, s BlobStore, d []byte, k string) (string, int64, error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.MkdirTemp(c.dir, ".fill-")
	if err != nil {
		return "", 0, err
	}
	budget := &inputBudget{}
	err = func() error {
		r, err := os.OpenRoot(tmp)
		if err != nil {
			return err
		}
		defer r.Close()
		return materializeContext(ctx, s, r, ".", d, 0, budget, nil)
	}()
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	final := filepath.Join(c.dir, k+"-"+randomSuffix())
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		workspace.RemoveAll(tmp)
		return "", 0, err
	}
	return final, int64(budget.bytes), nil
}

func randomSuffix() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *treeCache) release(e *treeEntry) {
	c.mu.Lock()
	e.refs--
	c.mu.Unlock()
	c.evict()
}

// evict deletes the least recently used unused trees while the cache is over its budget.
func (c *treeCache) evict() {
	for {
		c.mu.Lock()
		if c.total <= treeCacheMax {
			c.mu.Unlock()
			return
		}
		var victim string
		var ve *treeEntry
		for k, e := range c.entries {
			select {
			case <-e.ready:
			default:
				continue
			}
			if e.refs == 0 && (ve == nil || e.used < ve.used) {
				victim, ve = k, e
			}
		}
		if ve == nil {
			c.mu.Unlock()
			return // everything is in use
		}
		delete(c.entries, victim)
		c.total -= ve.size
		c.mu.Unlock()
		workspace.RemoveAll(ve.path)
	}
}

// inputPlan decides, while an exec root is materialized, which input directories come from
// the tree cache: they are bound read-only at their place (mounts) instead of copied.
type inputPlan struct {
	cache   *treeCache
	work    string   // the exec root on the host (mount destinations are under it)
	cwd     string   // the working dir (exec-root path): never inside a cached tree
	outputs []string // exec-root paths of the outputs: never inside or around a cached tree
	mounts  []sandbox.Mount
	held    []*treeEntry
}

// mount binds directory d (at path name in the exec root r) from the cache if it is large,
// was seen before, and holds no path the action must write. It reports whether it did.
func (p *inputPlan) mount(ctx context.Context, s BlobStore, r *os.Root, name string, d []byte, budget *inputBudget) (bool, error) {
	if len(p.mounts) >= treeMaxMounts {
		return false, nil
	}
	if within(p.cwd, name) {
		return false, nil
	}
	for _, o := range p.outputs {
		if within(o, name) || within(name, o) {
			return false, nil
		}
	}
	sz, err := p.cache.size(ctx, s, d, 0)
	if err != nil {
		return false, err
	}
	if sz.files < treeMinFiles && sz.bytes < treeMinBytes {
		return false, nil
	}
	k := treeKey(d)
	c := p.cache
	c.mu.Lock()
	if c.entries[k] == nil && !c.seen[k] {
		if len(c.seen) >= treeSeenMax {
			clear(c.seen)
		}
		c.seen[k] = true // cached the next time an action names it
		c.mu.Unlock()
		return false, nil
	}
	c.tick++
	tick := c.tick
	c.mu.Unlock()
	budget.nodes += sz.files
	budget.bytes += sz.bytes
	if budget.nodes > 100000 || budget.bytes > 2<<30 {
		return false, capacityError()
	}
	e, err := c.acquire(ctx, s, d)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	e.used = tick
	c.mu.Unlock()
	p.held = append(p.held, e)
	if err := r.Mkdir(name, 0o755); err != nil {
		return false, err
	}
	p.mounts = append(p.mounts, sandbox.Mount{Src: e.path, Dst: filepath.Join(p.work, name), RO: true})
	return true, nil
}

// done releases the plan's trees (after the action ended).
func (p *inputPlan) done() {
	for _, e := range p.held {
		p.cache.release(e)
	}
	p.held = nil
}

// within reports whether path p is dir or inside it (both clean and relative).
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
