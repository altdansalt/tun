package server

// Runners reuse the hub's session implementation, including workspace sync, sandbox
// agent, cancellation and BEP parsing. Only transport and lifecycle differ here.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

func (r *Runner) newSessionHost() error {
	s, err := New(Config{DataDir: filepath.Join(r.cfg.DataDir, "session-host"), Sandbox: r.cfg.Sandbox, PrivateNet: r.cfg.PrivateNet, MaxJobs: r.cfg.Slots, MinFree: r.cfg.MinFree, KillGrace: r.cfg.KillGrace, BazelCommand: r.cfg.BazelCommand, BazeliskSeed: r.cfg.BazeliskSeed, SessionIdle: 30 * time.Minute})
	if err != nil {
		return err
	}
	// The hub owns placement and eviction. Local expiry also bounds idle JVMs if
	// communication with the hub is lost; each lease supplies its idle interval.
	s.sessionDataDir = r.cfg.DataDir
	target, err := url.Parse(r.cfg.Hub)
	if err != nil {
		s.Close()
		return err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP2(true)
	tr.Protocols.SetUnencryptedHTTP2(true)
	r.sessionTransport = tr
	s.sessionProxy = func(sess *session) http.Handler {
		return &httputil.ReverseProxy{Transport: tr, Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.URL.Path = target.Path + "/v1/session-cache/" + sess.credential + p.In.URL.Path
			p.Out.Header.Del("X-Tun-Token")
			p.Out.Header.Del("Authorization")
			p.Out.Header.Del("X-Tun-Client")
		}}
	}
	r.sessions = s
	return nil
}

func (r *Runner) sessionLeases(ids []string) {
	s := r.sessions
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if !keep[id] && sess.job == nil && !sess.stopping {
			s.stopSession(sess, "hub revoked session lease")
		}
	}
}

func (r *Runner) fetchSessionBlobs(b *api.BazelJob) error {
	for _, sha := range r.sessions.missingBlobs(b.Manifest) {
		resp, err := r.do(r.ctx, "GET", "/v1/runner/cas/"+sha, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			err = hubError(resp)
			resp.Body.Close()
			return err
		}
		tmp, err := os.CreateTemp(filepath.Join(r.sessions.cfg.DataDir, "cas"), ".fetch-*")
		if err != nil {
			resp.Body.Close()
			return err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, r.sessions.cfg.MaxUpload+1))
		resp.Body.Close()
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil && hex.EncodeToString(h.Sum(nil)) != sha {
			err = fmt.Errorf("session blob %s failed digest verification", sha)
		}
		if err == nil {
			err = os.Rename(tmp.Name(), r.sessions.casPath(sha))
		}
		os.Remove(tmp.Name())
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) executeBazel(rj *runnerJob) (procResult, api.RunnerDone) {
	var done api.RunnerDone
	spec := rj.spec
	b := spec.Bazel
	s := r.sessions
	if err := r.fetchSessionBlobs(&b.Command); err != nil {
		return failed("session sync: %v", err), done
	}
	if err := prepareJobDir(s.cfg.DataDir, spec.ID); err != nil {
		return failed("%v", err), done
	}
	if err := r.retry(time.Minute, func() error { return r.postJSON(r.ctx, "/v1/runner/jobs/"+spec.ID+"/started", struct{}{}, nil) }); err != nil {
		return procResult{State: "crash"}, done
	}
	s.mu.Lock()
	for {
		old := s.sessions[b.Session.ID]
		if old == nil || (!old.stopping && old.CreatedAt.Equal(b.Session.CreatedAt)) {
			break
		}
		if !old.stopping && old.job == nil {
			s.stopSession(old, "new hub lease")
		}
		s.mu.Unlock()
		if r.ctx.Err() != nil {
			return procResult{State: "crash"}, done
		}
		sleepCtx(r.ctx, 10*time.Millisecond)
		s.mu.Lock()
	}
	sess := s.sessions[b.Session.ID]
	if sess == nil {
		v := b.Session
		v.Host = api.LocalRunner
		sess = &session{Session: v, loaded: true}
		s.sessions[v.ID] = sess
	}
	if sess.credential == "" {
		sess.credential = b.CacheCredential
		sess.cacheReadOnly = b.CacheReadOnly
		sess.idleSeconds = b.SessionIdleSec
	}
	j := &job{Job: api.Job{ID: spec.ID, Client: b.Session.Client, SessionID: b.Session.ID, Cwd: spec.Cwd, TimeoutSec: spec.TimeoutSec, State: api.Queued}, bz: &b.Command, runnerMaxLog: &spec.MaxLog, done: make(chan struct{})}
	s.jobs[j.ID] = j
	sess.job = j
	s.mu.Unlock()
	r.mu.Lock()
	rj.sessionJob = j
	if rj.killState != "" {
		j.cancelReq = true
	}
	r.mu.Unlock()
	s.reclaimDisk()
	var sent [2]int64
	stop, shipped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(shipped)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				r.shipSessionLogs(spec.ID, &sent)
			}
		}
	}()
	s.runBazel(j)
	close(stop)
	<-shipped
	done = api.RunnerDone{State: j.State, ExitCode: j.ExitCode, Signal: j.Signal, Error: j.Error, Summary: j.Summary}
	v := sess.Session
	done.Session = &v
	data, _ := os.ReadFile(filepath.Join(s.jobDir(j.ID), "bep.json"))
	if json.Valid(data) {
		done.BEP = data
	}
	r.retry(time.Minute, func() error { return r.postJSON(r.ctx, "/v1/runner/jobs/"+spec.ID+"/exited", done, nil) })
	if err := r.retry(time.Minute, func() error { return r.shipSessionLogs(spec.ID, &sent) }); err != nil {
		return procResult{State: "crash"}, done
	}
	s.mu.Lock()
	delete(s.jobs, j.ID)
	s.mu.Unlock()
	// Logs have reached the hub; the warm sandbox remains in the engine's session dir.
	os.RemoveAll(s.jobDir(j.ID))
	return procResult{State: j.State, Code: j.ExitCode, Sig: j.Signal, Msg: j.Error}, done
}

func (r *Runner) shipSessionLogs(id string, sent *[2]int64) error {
	return r.shipLogsDir(id, r.sessions.jobDir(id), sent)
}

func (r *Runner) cancelSessionJob(j *runnerJob, state string) {
	if j.sessionJob == nil {
		return
	}
	s := r.sessions
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.sessionJob.kill != nil {
		j.sessionJob.kill(state)
	} else {
		j.sessionJob.cancelReq = true
	}
}
