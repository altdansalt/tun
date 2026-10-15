package server

// Disk-aware retention. When free space on the data dir's filesystem is under MinFree, the
// server deletes the sandbox dirs of finished jobs, oldest first (as --keep-work would
// later), then idle sessions, least recently used first, until it is above again. It checks
// before it starts each job, before it unpacks a workspace, and with every retention sweep.
// A runner (which deletes a job's dir when the job ends) deletes its cached workspace blobs,
// least recently used first. healthz reports the state.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// MinFree is the free space to keep on the data dir's filesystem: Bytes, or Percent of its
// size. The zero value keeps none.
type MinFree struct {
	Bytes   int64
	Percent float64
}

func (m MinFree) set() bool { return m.Bytes > 0 || m.Percent > 0 }

func (m MinFree) bytes(total int64) int64 {
	if m.Percent > 0 {
		return int64(float64(total) * m.Percent / 100)
	}
	return m.Bytes
}

func (m MinFree) String() string {
	if m.Percent > 0 {
		return strconv.FormatFloat(m.Percent, 'f', -1, 64) + "%"
	}
	return fmtSize(m.Bytes)
}

// ParseMinFree parses "5GB" (sizes as for --max-upload), "15%", or "0" (none).
func ParseMinFree(v string) (MinFree, error) {
	v = strings.TrimSpace(v)
	if v == "0" || v == "" {
		return MinFree{}, nil
	}
	if p, ok := strings.CutSuffix(v, "%"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f <= 0 || f >= 100 {
			return MinFree{}, fmt.Errorf("bad percentage %q (want e.g. 15%%)", v)
		}
		return MinFree{Percent: f}, nil
	}
	n, err := parseSize(v)
	if err != nil {
		return MinFree{}, fmt.Errorf("%v, a percentage (15%%) or 0", err)
	}
	return MinFree{Bytes: n}, nil
}

// diskStat returns the free (for an unprivileged user) and total bytes of dir's filesystem.
// Tests replace it.
var diskStat = func(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * st.Bsize, int64(st.Blocks) * st.Bsize, nil
}

// diskInfo is the disk part of healthz (counters filled in by the caller).
func diskInfo(dir string, m MinFree) *api.Disk {
	free, total, err := diskStat(dir)
	if err != nil {
		return nil
	}
	d := &api.Disk{FreeBytes: free, TotalBytes: total, MinFreeBytes: m.bytes(total)}
	d.Low = m.set() && free < d.MinFreeBytes
	return d
}

// diskLow reports whether the data dir's filesystem has less free space than MinFree.
func (s *Server) diskLow() bool {
	d := diskInfo(s.cfg.DataDir, s.cfg.MinFree)
	return d != nil && d.Low
}

// reclaimDisk frees space while it is low: finished jobs' sandbox dirs (oldest first), then
// idle sessions (least recently used first).
func (s *Server) reclaimDisk() {
	if s.diskLow() {
		s.trimSharedCaches()
	}
	if !s.cfg.MinFree.set() || !s.diskLow() {
		return
	}
	s.gc.workMu.Lock()
	defer s.gc.workMu.Unlock()
	s.mu.Lock()
	var done []*job
	for _, j := range s.jobs {
		if api.Terminal(j.State) && j.EndedAt != nil && !j.WorkDeleted && s.gc.fails[s.sandboxDir(j.ID)] < maxRemoveTries {
			done = append(done, j)
		}
	}
	s.mu.Unlock()
	sort.Slice(done, func(a, b int) bool { return done[a].EndedAt.Before(*done[b].EndedAt) })
	works, sessions := 0, 0
	for _, j := range done {
		if !s.diskLow() {
			break
		}
		if s.deleteWork(j) {
			works++
		}
	}
	for s.diskLow() {
		s.mu.Lock()
		var victim *session
		for _, o := range s.sessions {
			if o.runner != nil {
				continue
			} // remote sessions cannot free the hub filesystem
			if o.job == nil && !o.stopping && !s.queuedFor(o) && (victim == nil || o.LastUsed.Before(victim.LastUsed)) {
				victim = o
			}
		}
		if victim == nil || s.closed {
			s.mu.Unlock()
			break
		}
		s.endSession(victim, fmt.Sprintf("deleted: disk space is low (tun serve --min-free %v)", s.cfg.MinFree))
		s.mu.Unlock()
		s.dropSession(victim)
		sessions++
	}
	s.gc.reclaimedWork.Add(int64(works))
	s.gc.reclaimedSessions.Add(int64(sessions))
	free, _, _ := diskStat(s.cfg.DataDir)
	log.Printf("tun: disk space is low (under --min-free %v): deleted %d finished jobs' work dirs and %d idle sessions; %s free now",
		s.cfg.MinFree, works, sessions, fmtBytes(free))
}

// deleteWork deletes finished job j's sandbox dir (and a runner job's collected outputs) and
// marks the job work_deleted. Caller holds s.gc.workMu.
func (s *Server) deleteWork(j *job) bool {
	if !s.remove(s.sandboxDir(j.ID)) {
		return false
	}
	os.Remove(filepath.Join(s.jobDir(j.ID), outputsFile)) // a runner job's files
	s.mu.Lock()
	if s.jobs[j.ID] == j {
		j.WorkDeleted = true
		if err := s.persist(j); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("tun: persisting job %s: %v", j.ID, err)
		}
	}
	s.mu.Unlock()
	return true
}

// fmtBytes is n in the largest unit that keeps it at least 1, with one decimal.
func fmtBytes(n int64) string {
	for _, u := range []struct {
		s string
		m int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.m {
			return fmt.Sprintf("%.1f%s", float64(n)/float64(u.m), u.s)
		}
	}
	return fmt.Sprintf("%dB", n)
}

// reclaimBlobs is the runner's reclaimDisk: workspace and session CAS blobs, least recently used first.
func (r *Runner) reclaimBlobs() {
	if !r.cfg.MinFree.set() {
		return
	}
	low := func() bool { d := diskInfo(r.cfg.DataDir, r.cfg.MinFree); return d != nil && d.Low }
	if !low() {
		return
	}
	dir := filepath.Join(r.cfg.DataDir, "blobs")
	type blob struct {
		name string
		used time.Time
	}
	var list []blob
	for _, base := range []string{dir, filepath.Join(r.cfg.DataDir, "session-host", "cas")} {
		ents, _ := os.ReadDir(base)
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
				list = append(list, blob{filepath.Join(base, e.Name()), fi.ModTime()})
			}
		}
	}
	sort.Slice(list, func(a, b int) bool { return list[a].used.Before(list[b].used) })
	n := 0
	for _, b := range list {
		if !low() {
			break
		}
		if os.Remove(b.name) == nil {
			n++
		}
	}
	r.reclaimed.Add(int64(n))
	free, _, _ := diskStat(r.cfg.DataDir)
	r.cfg.Logf("tun runner: disk space is low (under --min-free %v): deleted %d cached workspace/session blobs; %s free now", r.cfg.MinFree, n, fmtBytes(free))
}
