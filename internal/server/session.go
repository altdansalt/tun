package server

// Warm Bazel sessions (tun bazel). A session is identified by (client, workspace root on the
// client, session name) and lives in $DATA/sessions/<id>/:
//
//	session.json   the persisted api.Session
//	sync.json      the workspace.SyncState of the last sync into sandbox/work
//	tun.bazelrc    the flags tun adds (caches); read-only in the sandbox
//	agent.log      output of the session sandbox itself (bwrap, agent)
//	sandbox/       work (the synced workspace), home, tmp, out (Bazel's output user root)
//
// Under bwrap the session sandbox sees them at fixed paths (/work, /tun/home, /tmp,
// /tun/out, ...), so the output base and every path-keyed cache stay valid across commands.
// The sandbox (see sandbox/agent.go) lives between commands, and with it the Bazel server.
// Each tun bazel command is a job in the normal FIFO that takes a local slot and its
// session; a session runs one command at a time. Sessions expire after SessionIdle without a
// command; at most MaxSessions exist (the least recently used idle one is evicted for a new
// one). Session dirs survive a server restart; their Bazel servers don't.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/workspace"
)

// DefaultSession is the session name when the client gives none.
const DefaultSession = "default"

var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type session struct {
	api.Session                     // the persisted part
	job         *job                // the job holding the session (running, or about to)
	stopping    bool                // being stopped and deleted; no new commands until then
	sb          *sandbox.Session    // the live sandbox (and Bazel server), if any
	state       workspace.SyncState // owned by whoever holds the session; nil: load sync.json
	loaded      bool
}

// sessionID is the id of the session (client, root, name).
func sessionID(client, root, name string) string {
	h := sha256.Sum256([]byte(client + "\x00" + root + "\x00" + name))
	return hex.EncodeToString(h[:10])
}

func (s *Server) sessionDir(id string) string { return filepath.Join(s.cfg.DataDir, "sessions", id) }

// loadSessions reads the sessions a previous server left (their sandboxes are gone) and
// deletes half-removed ones.
func (s *Server) loadSessions() error {
	dir := filepath.Join(s.cfg.DataDir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".trash-") {
			workspace.RemoveAll(p)
			continue
		}
		f, err := openOwn(filepath.Join(p, "session.json"), os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		var sess session
		err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&sess.Session)
		f.Close()
		if err != nil || sess.ID != e.Name() {
			log.Printf("tun: ignoring session dir %s: bad session.json", p)
			continue
		}
		s.sessions[sess.ID] = &sess
	}
	return nil
}

func (s *Server) persistSession(sess *session) error {
	b, err := json.MarshalIndent(sess.Session, "", "  ")
	if err != nil {
		return err
	}
	return writeOwn(filepath.Join(s.sessionDir(sess.ID), "session.json"), b)
}

// writeOwn atomically replaces a server-owned file via a fresh temp file.
func writeOwn(name string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), name)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// checkBazelJob validates a bazel job request and fills in its session. It returns the
// HTTP status for a bad request.
func (s *Server) checkBazelJob(req *api.CreateJob, client string) (int, error) {
	b := req.Bazel
	switch {
	case len(req.Argv) > 0 || req.Script != "" || req.Workspace != "" || req.Service || len(req.Ports) > 0 || len(req.Outputs) > 0:
		return 400, errors.New("a bazel job has no argv, script, workspace, service, ports or outputs")
	case s.cfg.HubOnly:
		return 409, errors.New("tun bazel needs job slots on the server itself, and this one has none (tun serve --local-slots 0); sessions on runners are not supported yet")
	case len(b.Args) == 0:
		return 400, errors.New("bazel args are empty: give a bazel command, e.g. build //...")
	case strings.HasPrefix(b.Args[0], "-"):
		return 400, fmt.Errorf("bazel startup options (%q) are not supported: tun sets its own; start with the bazel command (build, test, query, ...)", b.Args[0])
	case !filepath.IsAbs(b.Root) || len(b.Root) > 4096:
		return 400, errors.New("bazel root must be the client's absolute workspace path")
	}
	if b.Session == "" {
		b.Session = DefaultSession
	}
	if !sessionNameRE.MatchString(b.Session) {
		return 400, fmt.Errorf("session name %q: use 1-64 letters, digits, '.', '_' or '-'", b.Session)
	}
	for _, a := range append(b.Args, b.Root) {
		if strings.Contains(a, "\x00") {
			return 400, errors.New("bazel args and root must not contain NUL bytes")
		}
	}
	if err := workspace.CheckManifest(b.Manifest, s.cfg.MaxWorkspace); err != nil {
		var tl *workspace.TooLargeError
		if errors.As(err, &tl) {
			return s.unpackErr(err)
		}
		return 400, fmt.Errorf("bad manifest: %v", err)
	}
	return 0, nil
}

// casPath is where file content with digest sha is stored (tun bazel's sync).
func (s *Server) casPath(sha string) string { return filepath.Join(s.cfg.DataDir, "cas", sha) }

// missingBlobs returns the manifest's file digests the server lacks, and marks the others
// used (an mtime older than an hour is bumped) so the sweep keeps them.
func (s *Server) missingBlobs(m []api.ManifestEntry) []string {
	s.gc.blobMu.Lock()
	defer s.gc.blobMu.Unlock()
	seen := map[string]bool{}
	missing := []string{}
	now := time.Now()
	for _, e := range m {
		if e.Type != api.EntryFile || seen[e.Sha] {
			continue
		}
		seen[e.Sha] = true
		fi, err := os.Stat(s.casPath(e.Sha))
		switch {
		case err != nil:
			missing = append(missing, e.Sha)
		case now.Sub(fi.ModTime()) > min(time.Hour, max(s.cfg.KeepBlobs/2, time.Second)):
			os.Chtimes(s.casPath(e.Sha), now, now)
		}
	}
	return missing
}

// putCAS serves POST /v1/cas: a tar (gzip-compressed if Content-Encoding: gzip) whose
// entries are file contents named by their sha256. Each is verified and stored.
func (s *Server) putCAS(w http.ResponseWriter, r *http.Request) {
	body := io.Reader(http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload))
	n, err := workspace.ReadBlobs(body, r.Header.Get("Content-Encoding") == "gzip", s.cfg.MaxUpload, filepath.Join(s.cfg.DataDir, "cas"))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe) || errors.Is(err, workspace.ErrBlobsTooLarge):
		writeErr(w, 413, fmt.Sprintf("blob upload is over the server limit of %s (tun serve --max-upload)", fmtSize(s.cfg.MaxUpload)))
	case err != nil:
		writeErr(w, 400, "blob upload: "+err.Error())
	default:
		writeJSON(w, 200, map[string]int{"stored": n})
	}
}

// claimSession gives queued bazel job j its session if it is free, creating the session
// (evicting the least recently used idle one if MaxSessions exist) if needed. It reports
// whether j can start now. Caller holds s.mu.
func (s *Server) claimSession(j *job) bool {
	if sess := s.sessions[j.SessionID]; sess != nil {
		if sess.job != nil || sess.stopping {
			return false
		}
		sess.job = j
		return true
	}
	if len(s.sessions) >= s.cfg.MaxSessions {
		var victim *session
		for _, o := range s.sessions {
			if o.stopping {
				return false // an eviction is under way; wait for it
			}
			if o.job == nil && !s.queuedFor(o) && (victim == nil || o.LastUsed.Before(victim.LastUsed)) {
				victim = o
			}
		}
		if victim != nil {
			s.stopSession(victim, fmt.Sprintf("evicted (least recently used of %d sessions) for a new session", s.cfg.MaxSessions))
		}
		return false
	}
	now := time.Now().UTC()
	b := j.bz
	sess := &session{Session: api.Session{ID: j.SessionID, Name: b.Session, Client: j.Client, Root: b.Root, CreatedAt: now, LastUsed: now}, loaded: true}
	s.sessions[sess.ID] = sess
	sess.job = j
	return true
}

// queuedFor reports whether a queued job waits for sess (it must not be evicted or expire
// under it). Caller holds s.mu.
func (s *Server) queuedFor(sess *session) bool {
	return slices.ContainsFunc(s.queue, func(j *job) bool { return j.SessionID == sess.ID })
}

// stopSession shuts down sess's Bazel server and sandbox and deletes it, in the background.
// Caller holds s.mu; sess must be idle.
func (s *Server) stopSession(sess *session, why string) {
	sess.stopping = true
	log.Printf("tun: session %s (%s, client %s): %s", sess.ID, sess.Name, sess.Client, why)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.shutdownSandbox(sess)
		s.removeSessionDir(sess.ID)
		s.mu.Lock()
		if s.sessions[sess.ID] == sess {
			delete(s.sessions, sess.ID)
		}
		s.schedule()
		s.mu.Unlock()
	}()
}

// removeSessionDir moves a session dir aside (so the id can be reused at once) and deletes it.
func (s *Server) removeSessionDir(id string) {
	dir := s.sessionDir(id)
	trash := filepath.Join(filepath.Dir(dir), ".trash-"+id+"-"+randHex(4))
	if err := os.Rename(dir, trash); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("tun: removing session %s: %v", id, err)
		}
		return
	}
	if err := workspace.RemoveAll(trash); err != nil {
		log.Printf("tun: removing session %s: %v", id, err)
	}
}

// expireSessions stops sessions idle for longer than SessionIdle.
func (s *Server) expireSessions(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, sess := range s.sessions {
		if sess.job == nil && !sess.stopping && now.Sub(sess.LastUsed) > s.cfg.SessionIdle && !s.queuedFor(sess) {
			s.stopSession(sess, fmt.Sprintf("idle for %v (tun serve --session-idle)", s.cfg.SessionIdle))
		}
	}
}

// listSessions serves GET /v1/sessions?client=NAME|mine=1, most recently used first.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	client := r.URL.Query().Get("client")
	if v := r.URL.Query().Get("mine"); v == "1" || v == "true" {
		client = clientOf(r)
	}
	s.mu.Lock()
	list := []api.Session{}
	for _, sess := range s.sessions {
		if client == "" || sess.Client == client {
			list = append(list, s.sessionView(sess))
		}
	}
	s.mu.Unlock()
	sort.Slice(list, func(a, b int) bool { return list[a].LastUsed.After(list[b].LastUsed) })
	writeJSON(w, 200, list)
}

// sessionView is sess's public object. Caller holds s.mu.
func (s *Server) sessionView(sess *session) api.Session {
	v := sess.Session
	switch {
	case sess.stopping:
		v.State = "stopping"
	case sess.job != nil:
		v.State, v.Job = "busy", sess.job.ID
	default:
		v.State = "idle"
	}
	if sess.sb != nil {
		select {
		case <-sess.sb.Done():
		default:
			v.Server = true
		}
	}
	v.ExpiresAt = v.LastUsed.Add(s.cfg.SessionIdle)
	return v
}

// queueReason says why queued bazel job j waits, if not just for a job slot. Caller holds s.mu.
func (s *Server) queueReason(j *job) string {
	sess := s.sessions[j.SessionID]
	switch {
	case sess != nil && sess.job != nil:
		return fmt.Sprintf("session %s is running job %s (commands in a session run one at a time; --session NAME for another)", sess.Name, sess.job.ID)
	case sess != nil && sess.stopping:
		return fmt.Sprintf("session %s is being stopped", sess.Name)
	case sess == nil && len(s.sessions) >= s.cfg.MaxSessions:
		return fmt.Sprintf("the server has its maximum of %d sessions; waiting until an idle one is evicted (tun serve --max-sessions)", s.cfg.MaxSessions)
	}
	return ""
}

// sessionPaths are a session's directories as the bazel command sees them.
type sessionPaths struct {
	work, home, tmp, out, repo, disk, bazelisk, rc, exe string
}

// sessionLayout returns the session's paths as the bazel command sees them (in) and on the
// host, and the spec of its sandbox.
func (s *Server) sessionLayout(sess *session) (in, host sessionPaths, spec sandbox.Spec) {
	dir := s.sessionDir(sess.ID)
	sb := filepath.Join(dir, "sandbox")
	ch := sha256.Sum256([]byte(sess.Client))
	host = sessionPaths{
		work: filepath.Join(sb, "work"), home: filepath.Join(sb, "home"), tmp: filepath.Join(sb, "tmp"),
		out: filepath.Join(sb, "out"), repo: s.cfg.BazelRepoCache,
		disk:     filepath.Join(s.cfg.DataDir, "cache", "bazel-disk", hex.EncodeToString(ch[:8])),
		bazelisk: s.cfg.BazeliskHome, rc: filepath.Join(dir, "tun.bazelrc"),
	}
	in = host
	spec = sandbox.Spec{Mode: s.cfg.Sandbox, Tmp: host.tmp, DataDir: s.cfg.DataDir}
	if s.cfg.Sandbox == sandbox.Bwrap {
		in = sessionPaths{work: "/work", home: "/tun/home", tmp: "/tmp", out: "/tun/out", repo: "/tun/cache/repo",
			disk: "/tun/cache/disk", bazelisk: "/tun/cache/bazelisk", rc: "/tun/tun.bazelrc", exe: "/tun/bin/tun"}
		spec.FixedRoot, spec.PrivateNet, spec.ExePath = true, s.cfg.PrivateNet, in.exe
		spec.Mounts = []sandbox.Mount{
			{Src: host.work, Dst: in.work}, {Src: host.home, Dst: in.home}, {Src: host.out, Dst: in.out},
			{Src: host.repo, Dst: in.repo}, {Src: host.disk, Dst: in.disk}, {Src: host.bazelisk, Dst: in.bazelisk},
			{Src: host.rc, Dst: in.rc, RO: true},
		}
	}
	spec.Argv, spec.Dir = sandbox.AgentArgv(in.exe), in.work
	env := jobEnv(s.cfg.DataDir, "", nil)
	spec.Env = envList(map[string]string{"PATH": env["PATH"], "LANG": env["LANG"], "TERM": "dumb", "USER": env["USER"],
		"HOME": in.home, "TMPDIR": in.tmp, "BAZELISK_HOME": in.bazelisk, "TUN_SESSION": sess.Name})
	return in, host, spec
}

// startupOptions are the bazel startup options tun adds (they must not change between
// commands of a session, or Bazel restarts its server).
func (s *Server) startupOptions(p sessionPaths) []string {
	return []string{"--output_user_root=" + p.out, "--output_base=" + p.out + "/base", "--bazelrc=" + p.rc,
		fmt.Sprintf("--max_idle_secs=%d", int((s.cfg.SessionIdle + 5*time.Minute).Seconds()))}
}

// prepareSession creates the session's dirs and files if needed.
func (s *Server) prepareSession(sess *session, in, host sessionPaths) error {
	dir := s.sessionDir(sess.ID)
	for _, d := range []string{"", "sandbox", "sandbox/work", "sandbox/home", "sandbox/tmp", "sandbox/out"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	for _, d := range []string{host.repo, host.disk, host.bazelisk} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	rc := fmt.Sprintf("# Written by tun for session %s; tun passes it with --bazelrc.\n"+
		"common --repository_cache=%s\ncommon --disk_cache=%s\ncommon --experimental_disk_cache_gc_max_size=%s\n",
		sess.Name, in.repo, in.disk, s.cfg.DiskCacheMax)
	if err := writeOwn(host.rc, []byte(rc)); err != nil {
		return err
	}
	return s.persistSession(sess)
}

// ensureSandbox starts the session's sandbox (and so, at the first command, its Bazel
// server) unless it is running.
func (s *Server) ensureSandbox(sess *session, spec sandbox.Spec) (bool, error) {
	if sess.sb != nil {
		select {
		case <-sess.sb.Done():
		default:
			return false, nil
		}
	}
	dir := s.sessionDir(sess.ID)
	if s.cfg.Sandbox == sandbox.Bwrap {
		// A previous sandbox's Bazel server is gone with its pid namespace; its pid/port files
		// would name processes of another namespace. (Without bwrap they are host pids, and a
		// server that outlived a tun restart is still valid.)
		// out is writable by the sandbox: never follow its links (os.Root).
		if root, err := os.OpenRoot(filepath.Join(dir, "sandbox", "out")); err == nil {
			root.RemoveAll("base/server")
			root.Close()
		}
	}
	// A new log per sandbox (O_TRUNC), so it can't grow over the life of the session.
	f, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sb, err := sandbox.StartSession(spec, f)
	if err != nil {
		return false, fmt.Errorf("starting the session sandbox: %v", err)
	}
	s.mu.Lock()
	sess.sb = sb
	s.mu.Unlock()
	return true, nil
}

// shutdownSandbox asks the session's Bazel server to shut down and ends the sandbox.
func (s *Server) shutdownSandbox(sess *session) {
	sb := sess.sb
	if sb == nil {
		return
	}
	select {
	case <-sb.Done():
	default:
		if f, err := os.OpenFile(filepath.Join(s.sessionDir(sess.ID), "agent.log"), os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0); err == nil {
			in, _, spec := s.sessionLayout(sess)
			argv := append(append([]string{s.cfg.BazelCommand}, s.startupOptions(in)...), "shutdown")
			if res, err := sb.Run(argv, spec.Dir, f, f); err == nil {
				select {
				case <-res:
				case <-time.After(time.Minute):
				}
			}
			f.Close()
		}
		sb.Close(5 * time.Second)
	}
	s.mu.Lock()
	if sess.sb == sb {
		sess.sb = nil
	}
	s.mu.Unlock()
}

// bazelRun is the kill state of one running bazel job.
type bazelRun struct {
	mu       sync.Mutex
	endState string // set by the first kill
	sb       *sandbox.Session
	killed   chan struct{}
	done     chan struct{} // closed once the command has ended
}

// kill asks the bazel job to end in state ("" = server shutdown: crash). Bazel's client
// gets SIGINT (it cancels the command in its server), then SIGKILL after the grace period;
// if even that doesn't end it, the session sandbox is killed.
func (br *bazelRun) kill(state string, grace time.Duration) {
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.endState != "" {
		return
	}
	br.endState = state
	if state == "" {
		br.endState = "crash"
	}
	close(br.killed)
	if br.sb != nil {
		br.escalate(state == "", grace)
	}
}

// escalate ends the started command: at once with the sandbox (crash), else SIGINT, SIGKILL
// after grace, the sandbox after twice that unless the command ended. Caller holds br.mu.
func (br *bazelRun) escalate(crash bool, grace time.Duration) {
	sb := br.sb
	if crash {
		go sb.Kill()
		return
	}
	sb.Signal(syscall.SIGINT)
	later := func(f func()) func() {
		return func() {
			select {
			case <-br.done:
			default:
				f()
			}
		}
	}
	time.AfterFunc(grace, later(func() { sb.Signal(syscall.SIGKILL) }))
	time.AfterFunc(2*grace, later(sb.Kill))
}

// runBazel runs bazel job j, which holds its session: sync, then the command in the
// session sandbox.
func (s *Server) runBazel(j *job) {
	s.mu.Lock()
	sess := s.sessions[j.SessionID]
	br := &bazelRun{killed: make(chan struct{}), done: make(chan struct{})}
	logs, err := openLogs(s.jobDir(j.ID), s.cfg.MaxLog)
	if err == nil && s.closed {
		logs.close()
		err = errors.New("server is shutting down")
	}
	if err == nil && j.cancelReq {
		logs.close()
		s.finish(j, api.Canceled, nil, nil, "canceled before it started")
		s.releaseSession(sess, j, false)
		s.mu.Unlock()
		return
	}
	if err != nil {
		s.finish(j, api.Failed, nil, nil, err.Error())
		s.releaseSession(sess, j, false)
		s.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	j.State, j.StartedAt = api.Running, &now
	s.persist(j)
	j.kill = func(state string) { br.kill(state, s.cfg.KillGrace) }
	s.mu.Unlock()

	timer := time.AfterFunc(time.Duration(j.TimeoutSec)*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if j.kill != nil {
			j.kill(api.Timeout)
		}
	})
	res := s.bazelCommand(j, sess, br, logs)
	close(br.done)
	timer.Stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	br.mu.Lock()
	state := br.endState
	br.mu.Unlock()
	switch state {
	case "crash":
		j.kill = nil // the next server marks it failed
		sess.job = nil
		return
	case api.Timeout:
		res.State, res.Msg = api.Timeout, fmt.Sprintf("timed out after %ds", j.TimeoutSec)
	case api.Canceled:
		res.State, res.Msg = api.Canceled, "canceled"
	}
	s.finish(j, res.State, res.Code, res.Sig, res.Msg)
	s.releaseSession(sess, j, true)
}

// releaseSession records a command in sess and frees it for the next one. Caller holds s.mu.
func (s *Server) releaseSession(sess *session, j *job, ran bool) {
	if sess == nil || sess.job != j {
		return
	}
	sess.job = nil
	if ran {
		sess.LastUsed, sess.LastJob = time.Now().UTC(), j.ID
		sess.Commands++
		if err := s.persistSession(sess); err != nil {
			log.Printf("tun: session %s: %v", sess.ID, err)
		}
	}
	s.schedule()
}

// bazelCommand does the work of runBazel without s.mu: --cold, sync, sandbox, command.
func (s *Server) bazelCommand(j *job, sess *session, br *bazelRun, logs *jobLogs) procResult {
	say := func(format string, a ...any) { fmt.Fprintf(logs.Stderr, "tun: "+format+"\n", a...) }
	endLogs := func() {
		logs.started()
		logs.wait()
	}
	fail := func(format string, a ...any) procResult {
		msg := fmt.Sprintf(format, a...)
		say("%s", msg)
		endLogs()
		return procResult{State: api.Failed, Msg: msg}
	}
	killed := func() bool {
		select {
		case <-br.killed:
			return true
		default:
			return false
		}
	}
	b := j.bz
	if b.Cold && (sess.Commands > 0 || sess.sb != nil) {
		s.shutdownSandbox(sess)
		s.removeSessionDir(sess.ID)
		s.mu.Lock()
		sess.Commands, sess.CreatedAt, sess.state, sess.loaded = 0, time.Now().UTC(), nil, true
		s.mu.Unlock()
		say("--cold: discarded session %s's state and Bazel server", sess.Name)
	}
	p, hp, spec := s.sessionLayout(sess)
	if err := s.prepareSession(sess, p, hp); err != nil {
		return fail("preparing session %s: %v", sess.Name, err)
	}
	if !sess.loaded {
		if b, err := os.ReadFile(filepath.Join(s.sessionDir(sess.ID), "sync.json")); err == nil {
			json.Unmarshal(b, &sess.state)
		}
		sess.loaded = true
	}
	start := time.Now()
	state, st, err := workspace.Apply(hp.work, sess.state, b.Manifest, func(sha string) (*os.File, error) {
		f, err := os.Open(s.casPath(sha))
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("content %s is no longer on the server (deleted by retention); run the command again", sha)
		}
		return f, err
	})
	sess.state = state
	if sb, merr := json.Marshal(state); merr == nil {
		if werr := writeOwn(filepath.Join(s.sessionDir(sess.ID), "sync.json"), sb); werr != nil && err == nil {
			err = werr
		}
	}
	if err != nil {
		return fail("syncing the workspace into session %s: %v", sess.Name, err)
	}
	say("session %s (%s): synced workspace in %.3fs: %v", sess.Name, sess.ID, time.Since(start).Seconds(), st)
	dir := path.Join(p.work, j.Cwd)
	if fi, err := os.Stat(filepath.Join(hp.work, j.Cwd)); err != nil || !fi.IsDir() {
		return fail("cwd %q does not exist in the workspace", j.Cwd)
	}
	if killed() {
		endLogs()
		return procResult{State: api.Canceled}
	}
	started, err := s.ensureSandbox(sess, spec)
	if err != nil {
		return fail("%v", err)
	}
	if started {
		say("session %s: started a new session sandbox (Bazel server starts with this command)", sess.Name)
	}
	argv := append(append([]string{s.cfg.BazelCommand}, s.startupOptions(p)...), b.Args...)
	if killed() {
		endLogs()
		return procResult{State: api.Canceled}
	}
	// Not under br.mu: Run may wait for the agent, and kill is called with s.mu held.
	resc, err := sess.sb.Run(argv, dir, logs.Stdout, logs.Stderr)
	if err != nil {
		return fail("session %s: %v", sess.Name, err)
	}
	br.mu.Lock()
	br.sb = sess.sb
	if br.endState != "" { // killed while starting: deliver it now
		br.escalate(br.endState == "crash", s.cfg.KillGrace)
	}
	br.mu.Unlock()
	logs.started()
	r := <-resc
	logs.wait()
	if r.Err != nil {
		tail := ""
		if f, err := openOwn(filepath.Join(s.sessionDir(sess.ID), "agent.log"), os.O_RDONLY, 0); err == nil {
			if off, err := tailStart(f, 5); err == nil {
				b := make([]byte, 4096)
				n, _ := f.ReadAt(b, off)
				tail = strings.ReplaceAll(strings.TrimSpace(string(b[:n])), "\n", " | ")
			}
			f.Close()
		}
		return procResult{State: api.Failed, Msg: fmt.Sprintf("session %s: %v (agent.log: %s)", sess.Name, r.Err, tail)}
	}
	return procResult{State: api.Exited, Code: r.Code, Sig: r.Signal}
}

// closeSessions kills every session sandbox (server shutdown). Session dirs stay.
func (s *Server) closeSessions() {
	s.mu.Lock()
	var sbs []*sandbox.Session
	for _, sess := range s.sessions {
		if sess.sb != nil {
			sbs = append(sbs, sess.sb)
		}
	}
	s.mu.Unlock()
	for _, sb := range sbs {
		sb.Kill()
	}
}
