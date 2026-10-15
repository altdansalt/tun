package server

// Storage retention ("gc"): a sweep, run at startup and then periodically, that
//   - deletes the work/, home/ and tmp/ dirs of jobs that ended more than KeepWork ago
//     (job.json and the stdout/stderr logs stay; the job gets work_deleted=true),
//   - deletes the whole job dir (and forgets the job) once it ended more than KeepJobs ago,
//   - deletes blobs not referenced by a queued/running job and unused for KeepBlobs.
// A blob's "last used" time is its mtime, bumped by HEAD, PUT and job creation, so it survives
// server restarts. A zero Keep* duration disables that kind of deletion.

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

// orphanAge is how old a job dir without a loaded job must be before the sweep deletes it.
// Job dirs exist briefly before their job is registered (while the workspace unpacks), so
// this is deliberately long.
const orphanAge = time.Hour

type gcState struct {
	blobMu sync.Mutex // serializes blob deletion against HEAD/job creation touching blobs
	stop   chan struct{}
	done   chan struct{}
}

// gcInterval is how often the sweep runs: GCInterval if set, else a quarter of the shortest
// retention, clamped to [1s, 10m].
func (c *Config) gcInterval() time.Duration {
	if c.GCInterval > 0 {
		return c.GCInterval
	}
	d := 10 * time.Minute
	for _, k := range []time.Duration{c.KeepWork, c.KeepJobs, c.KeepBlobs} {
		if k > 0 && k/4 < d {
			d = k / 4
		}
	}
	return max(d, time.Second)
}

// startGC runs the first sweep now and then every gcInterval until stopGC.
func (s *Server) startGC() {
	if s.cfg.KeepWork <= 0 && s.cfg.KeepJobs <= 0 && s.cfg.KeepBlobs <= 0 {
		return
	}
	s.gc.stop, s.gc.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.gc.done)
		t := time.NewTicker(s.cfg.gcInterval())
		defer t.Stop()
		for {
			s.sweep(time.Now())
			select {
			case <-s.gc.stop:
				return
			case <-t.C:
			}
		}
	}()
}

func (s *Server) stopGC() {
	if s.gc.stop != nil {
		close(s.gc.stop)
		<-s.gc.done
		s.gc.stop = nil
	}
}

// sweep applies the retention policy once.
func (s *Server) sweep(now time.Time) {
	var expired, workGone []*job
	inUse := map[string]bool{}
	s.mu.Lock()
	for id, j := range s.jobs {
		if !api.Terminal(j.State) {
			inUse[j.Workspace] = true
			continue
		}
		if j.EndedAt == nil {
			continue
		}
		age := now.Sub(*j.EndedAt)
		switch {
		case s.cfg.KeepJobs > 0 && age > s.cfg.KeepJobs:
			delete(s.jobs, id)
			expired = append(expired, j)
		case s.cfg.KeepWork > 0 && age > s.cfg.KeepWork && !j.WorkDeleted:
			workGone = append(workGone, j)
		}
	}
	known := make(map[string]bool, len(s.jobs))
	for id := range s.jobs {
		known[id] = true
	}
	s.mu.Unlock()

	for _, j := range expired {
		if err := removeAll(s.jobDir(j.ID)); err != nil {
			log.Printf("tun: gc: deleting job %s: %v", j.ID, err)
		}
	}
	for _, j := range workGone {
		var errs []error
		for _, d := range []string{"work", "home", "tmp"} {
			errs = append(errs, removeAll(filepath.Join(s.jobDir(j.ID), d)))
		}
		if err := errors.Join(errs...); err != nil {
			log.Printf("tun: gc: deleting work dirs of job %s: %v", j.ID, err)
			continue // retried next sweep
		}
		s.mu.Lock()
		if s.jobs[j.ID] == j {
			j.WorkDeleted = true
			if err := s.persist(j); err != nil {
				log.Printf("tun: gc: persisting job %s: %v", j.ID, err)
			}
		}
		s.mu.Unlock()
	}
	if s.cfg.KeepJobs > 0 {
		s.sweepOrphans(now, known)
	}
	if s.cfg.KeepBlobs > 0 {
		s.sweepBlobs(now, inUse)
	}
}

// sweepOrphans deletes job dirs that no loaded job owns (half-created, or unreadable job.json).
func (s *Server) sweepOrphans(now time.Time, known map[string]bool) {
	jobsDir := filepath.Join(s.cfg.DataDir, "jobs")
	ents, err := os.ReadDir(jobsDir)
	if err != nil {
		log.Printf("tun: gc: %v", err)
		return
	}
	for _, e := range ents {
		if known[e.Name()] {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > max(s.cfg.KeepJobs, orphanAge) {
			s.mu.Lock()
			_, loaded := s.jobs[e.Name()] // registered since we looked
			s.mu.Unlock()
			if !loaded {
				if err := removeAll(filepath.Join(jobsDir, e.Name())); err != nil {
					log.Printf("tun: gc: deleting orphan job dir %s: %v", e.Name(), err)
				}
			}
		}
	}
}

// sweepBlobs deletes blobs (and abandoned partial uploads) unused for KeepBlobs, except
// workspaces of queued/running jobs.
func (s *Server) sweepBlobs(now time.Time, inUse map[string]bool) {
	dir := filepath.Join(s.cfg.DataDir, "blobs")
	ents, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("tun: gc: %v", err)
		return
	}
	s.gc.blobMu.Lock()
	defer s.gc.blobMu.Unlock()
	for _, e := range ents {
		sha, isBlob := strings.CutSuffix(e.Name(), ".tar.gz")
		if isBlob && inUse[sha] {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) <= s.cfg.KeepBlobs {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("tun: gc: deleting blob %s: %v", e.Name(), err)
		}
	}
}

// useBlob reports whether blob sha exists and, if so, marks it used now. Callers use the
// result immediately; the sweep cannot delete a blob between the check and the touch.
func (s *Server) useBlob(sha string) bool {
	s.gc.blobMu.Lock()
	defer s.gc.blobMu.Unlock()
	now := time.Now()
	return os.Chtimes(s.blobPath(sha), now, now) == nil
}

// openBlob opens blob sha and marks it used.
func (s *Server) openBlob(sha string) (*os.File, error) {
	s.gc.blobMu.Lock()
	defer s.gc.blobMu.Unlock()
	f, err := os.Open(s.blobPath(sha))
	if err == nil {
		now := time.Now()
		os.Chtimes(s.blobPath(sha), now, now)
	}
	return f, err
}

// unpackErr turns an unpack error into an HTTP status and an actionable message.
func (s *Server) unpackErr(err error) (int, error) {
	var tl *workspace.TooLargeError
	if errors.As(err, &tl) {
		return 413, fmt.Errorf("workspace unpacks to more than %s, over the server limit (tun serve --max-workspace; crossed at %q); shrink it (gitignore build outputs and large data files) or use --no-workspace", fmtSize(tl.Limit), tl.Entry)
	}
	return 400, fmt.Errorf("unpacking workspace: %v", err)
}

// noSuchJob is the 404 message for an unknown job id.
func (s *Server) noSuchJob(id string) string {
	if s.cfg.KeepJobs > 0 {
		return fmt.Sprintf("no such job %q (finished jobs are deleted %v after they end; tun serve --keep-jobs)", id, s.cfg.KeepJobs)
	}
	return fmt.Sprintf("no such job %q", id)
}

func fmtSize(n int64) string {
	for _, u := range []struct {
		s string
		m int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.m && n%u.m == 0 {
			return fmt.Sprintf("%d%s", n/u.m, u.s)
		}
	}
	return fmt.Sprintf("%d bytes", n)
}

// removeAll is os.RemoveAll, retried after making every directory writable (jobs can leave
// read-only trees behind, e.g. a Go module cache under $HOME).
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
