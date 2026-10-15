package server

// Session placement and remote leases belong to the hub. The runner executes the same
// session engine; the hub keeps its public metadata and binds it to a runner instance.
import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

var sessionRPC = regexp.MustCompile("^/[A-Za-z0-9_.]+/[A-Za-z]+$")

const sessionMemory = int64(1536 << 20)

func hostResources() (available int64, load float64) {
	b, _ := os.ReadFile("/proc/meminfo")
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemAvailable:" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			available = n * 1024
		}
	}
	b, _ = os.ReadFile("/proc/loadavg")
	if f := strings.Fields(string(b)); len(f) > 0 {
		load, _ = strconv.ParseFloat(f[0], 64)
	}
	load /= float64(runtime.NumCPU())
	return
}

func (s *Server) hostSessions(host string) int {
	n := 0
	for _, sess := range s.sessions {
		if sess.Host == host {
			n++
		}
	}
	return n
}

// sessionHost preserves affinity even when another machine becomes less busy. New
// sessions prefer spare capacity to eviction, then memory and load. Unknown memory
// (older hosts) is not assumed to have space for a JVM. A busy host can queue work.
// Caller holds s.mu.
func (s *Server) sessionHost(j *job) string {
	if sess := s.sessions[j.SessionID]; sess != nil {
		if sess.runner != nil && s.runners[sess.Host] != sess.runner {
			return ""
		}
		return sess.Host
	}
	// Pin an eviction to one host so fresh resource samples cannot revoke a
	// second host's idle JVM while the first shutdown is still in progress.
	if j.placementHost != "" {
		if j.placementHost == api.LocalRunner && !s.cfg.HubOnly {
			return j.placementHost
		}
		if rn := s.runners[j.placementHost]; rn != nil && rn == j.placementRunner && rn.slots > 0 && time.Since(rn.lastSeen) <= s.cfg.RunnerDeadAfter {
			return j.placementHost
		}
		j.placementHost, j.placementRunner = "", nil
	}
	best := ""
	score := -1e30
	bestFree := false
	consider := func(host string, mem int64, load float64, busy, slots int, disk *api.Disk) {
		if slots <= 0 {
			return
		}
		n := s.hostSessions(host)
		// Reserve for leases assigned since the last resource sample. Existing JVMs are
		// already reflected in MemAvailable; don't subtract their memory a second time.
		pending := 0
		for _, sess := range s.sessions {
			if sess.Host == host && sess.Commands == 0 {
				pending++
			}
		}
		mem -= int64(pending) * sessionMemory
		if disk != nil && disk.FreeBytes < disk.MinFreeBytes {
			return
		}
		v := float64(mem)/float64(sessionMemory) - load - float64(busy)/float64(slots)
		if n >= s.sessionCapacity(host) {
			v -= 1000
		} else {
			v += 100
		}
		v -= float64(n) * 2
		free := busy < slots
		if best == "" || (free && !bestFree) || (free == bestFree && (v > score || (v == score && host < best))) {
			bestFree = free
			best, score = host, v
		}
	}
	if !s.cfg.HubOnly {
		mem, load, disk := s.localSessionResources()
		consider(api.LocalRunner, mem, load, s.running, s.cfg.MaxJobs, disk)
	}
	for _, rn := range s.runners {
		if rn.sessionSupport && time.Since(rn.lastSeen) <= s.cfg.RunnerDeadAfter {
			consider(rn.name, rn.memoryAvailable, rn.load, len(rn.jobs), rn.slots, rn.disk)
		}
	}
	return best
}

func (s *Server) runnerSessionLeases(rn *runnerState) []string {
	out := []string{}
	for _, sess := range s.sessions {
		if sess.runner == rn && !sess.stopping {
			out = append(out, sess.ID)
		}
	}
	return out
}

func (s *Server) hasSessionCache() bool { return s.cache != nil || s.sessionProxy != nil }
func (s *Server) sessionCacheHandler(sess *session) http.Handler {
	if s.sessionProxy != nil {
		return s.sessionProxy(sess)
	}
	return s.cache.ForClient(sess.Client)
}

// This credential authorizes only REAPI for one session's client namespace, never the
// ordinary tun API. Removing the lease revokes it, including after runner loss.
func (s *Server) remoteSessionCache(w http.ResponseWriter, r *http.Request) {
	credential := r.Header.Get("X-Tun-Session-Credential")
	s.mu.Lock()
	client := ""
	sess := s.sessions[r.Header.Get("X-Tun-Session")]
	if sess != nil && sess.runner != nil && !sess.stopping && credential != "" && subtle.ConstantTimeCompare([]byte(sess.credential), []byte(credential)) == 1 {
		client = sess.Client
	}
	s.mu.Unlock()
	if client == "" || s.cache == nil {
		writeErr(w, 403, "expired session cache credential")
		return
	}
	r.URL.Path = "/" + r.PathValue("rpc")
	if !sessionRPC.MatchString(r.URL.Path) {
		writeErr(w, 400, "invalid REAPI method")
		return
	}
	s.cache.ForClient(client).ServeHTTP(w, r)
}

func (s *Server) runnerCAS(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !shaRE.MatchString(sha) {
		writeErr(w, 400, "invalid CAS digest")
		return
	}
	f, err := os.Open(s.casPath(sha))
	if err != nil {
		writeErr(w, 404, fmt.Sprintf("missing CAS blob %s", sha))
		return
	}
	defer f.Close()
	http.ServeContent(w, r, sha, time.Time{}, f)
}

// Grow only when a sampled host can afford another JVM; otherwise reuse its
// existing budget by evicting an idle session. Always allow one session so a host
// with temporarily low memory can make progress instead of queueing forever.
func (s *Server) sessionCapacity(host string) int {
	mem, _, _ := s.localSessionResources()
	if host != api.LocalRunner {
		if rn := s.runners[host]; rn != nil {
			mem = rn.memoryAvailable
		}
	}
	if mem == 0 || s.cfg.SessionMemory < 0 {
		return s.cfg.MaxSessions
	}
	live, pending := 0, 0
	for _, sess := range s.sessions {
		if sess.Host == host {
			if sess.Commands == 0 {
				pending++
			} else {
				live++
			}
		}
	}
	return max(1, min(s.cfg.MaxSessions, live+int(max(int64(0), mem-int64(pending)*sessionMemory)/sessionMemory)+pending))
}

func (s *Server) bazelLogLimit(j *job) int64 {
	if j.runnerMaxLog != nil {
		return *j.runnerMaxLog
	}
	return s.cfg.MaxLog
}

func (s *Server) sessionIdle(sess *session) time.Duration {
	if sess.idleSeconds > 0 {
		return time.Duration(sess.idleSeconds) * time.Second
	}
	return s.cfg.SessionIdle
}

// Cache OS samples independently of queue length. Caller holds s.mu.
func (s *Server) localSessionResources() (int64, float64, *api.Disk) {
	if time.Since(s.resourceSample) > time.Second {
		s.resourceMemory, s.resourceLoad = hostResources()
		s.resourceDisk = diskInfo(s.cfg.DataDir, s.cfg.MinFree)
		s.resourceSample = time.Now()
	}
	return s.resourceMemory, s.resourceLoad, s.resourceDisk
}
func (s *Server) runnerCASBatch(w http.ResponseWriter, r *http.Request) {
	var digests []string
	if err := json.NewDecoder(io.LimitReader(r.Body, 32768)).Decode(&digests); err != nil || len(digests) > 256 {
		writeErr(w, 400, "want at most 256 digests")
		return
	}
	files := make(map[string]string, len(digests))
	for _, sha := range digests {
		if !shaRE.MatchString(sha) {
			writeErr(w, 400, "invalid CAS digest")
			return
		}
		files[sha] = sha
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := workspace.WriteBlobs(w, filepath.Join(s.cfg.DataDir, "cas"), files); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// Do not long-poll while a reported sandbox needs revocation: its removal frees
// capacity only on a later poll, after the runner has finished stopping it.
func (s *Server) runnerNeedsRevocation(rn *runnerState, reported []string) bool {
	// A command can finish and trigger eviction while this poll waits, after
	// the runner took its session snapshot. Send the revocation immediately.
	for _, sess := range s.sessions {
		if sess.runner == rn && sess.stopping {
			return true
		}
	}

	for _, id := range reported {
		sess := s.sessions[id]
		if sess == nil || sess.runner != rn || sess.stopping {
			return true
		}
	}
	return false
}
