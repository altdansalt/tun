package server

// Runners reuse the hub's session implementation, including workspace sync, sandbox
// agent, cancellation and BEP parsing. Only transport and lifecycle differ here.
import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
	"github.com/altdansalt/tun/internal/workspace"
)

func (r *Runner) newSessionHost() error {
	s, err := New(Config{DataDir: filepath.Join(r.cfg.DataDir, "session-host"), Sandbox: r.cfg.Sandbox, PrivateNet: r.cfg.PrivateNet, MaxJobs: r.cfg.Slots, MinFree: r.cfg.MinFree, KillGrace: r.cfg.KillGrace, BazelCommand: r.cfg.BazelCommand, BazeliskSeed: r.cfg.BazeliskSeed, KeepBlobs: r.cfg.KeepBlobs, KeepJobs: time.Hour, SessionIdle: 30 * time.Minute, MaxClientCache: r.cfg.MaxClientCache})
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
	tr.Protocols.SetHTTP1(true)
	// HTTP/1 means one connection per concurrent RPC; keep a Bazel-sized pool warm so cache
	// bursts don't pay a TLS handshake through the proxy per call.
	tr.MaxIdleConnsPerHost = 64
	r.sessionTransport = tr
	// HTTP upgrades require HTTP/1.1, including when the hub advertises h2 over TLS.
	base := http.DefaultTransport.(*http.Transport)
	if r.cfg.HTTP != nil {
		if t, ok := r.cfg.HTTP.Transport.(*http.Transport); ok {
			base = t
		}
	}
	st := base.Clone()
	st.Protocols = new(http.Protocols)
	st.Protocols.SetHTTP1(true)
	if st.TLSClientConfig != nil {
		st.TLSClientConfig = st.TLSClientConfig.Clone()
	} else {
		st.TLSClientConfig = new(tls.Config)
	}
	st.TLSClientConfig.NextProtos = []string{"http/1.1"}
	r.serviceTransport = st
	r.serviceHTTP = &http.Client{Transport: st}

	s.sessionProxy = func(sess *session) http.Handler {
		proxy := &httputil.ReverseProxy{Transport: reapi.WebTransport{Base: tr}, ModifyResponse: func(resp *http.Response) error { resp.Header.Del("Location"); return nil }, Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.URL.Path = target.Path + "/v1/session-cache" + p.In.URL.Path
			p.Out.URL.RawPath = ""
			p.Out.URL.RawQuery = ""
			p.Out.Header.Set("X-Tun-Session", sess.ID)
			p.Out.Header.Set("X-Tun-Session-Credential", sess.credential)
			p.Out.Header.Del("X-Tun-Token")
			p.Out.Header.Del("Authorization")
			p.Out.Header.Del("X-Tun-Client")
		}}
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !sessionRPC.MatchString(req.URL.Path) || req.URL.RawPath != "" {
				writeErr(w, 400, "invalid REAPI method")
				return
			}
			proxy.ServeHTTP(w, req)
		})
	}
	s.sessionSync = func(j *job, killed <-chan struct{}) error {
		ctx, cancel := context.WithCancel(r.ctx)
		defer cancel()
		go func() {
			select {
			case <-killed:
				cancel()
			case <-ctx.Done():
			}
		}()
		limit := j.runnerMaxUpload
		if limit <= 0 {
			limit = s.cfg.MaxUpload
		}
		return r.fetchSessionBlobsLimit(ctx, j.bz, limit)
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

func (r *Runner) fetchSessionBlobs(ctx context.Context, b *api.BazelJob) error {
	return r.fetchSessionBlobsLimit(ctx, b, r.sessions.cfg.MaxUpload)
}
func (r *Runner) fetchSessionBlobsLimit(ctx context.Context, b *api.BazelJob, limit int64) error {
	// The hub checked uploader ownership before admitting this command. This
	// private CAS is an executor cache, so fetching needs no second ownership check.
	missing := r.sessions.missingBlobs("", b.Manifest)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var first error
	var mu sync.Mutex
	batches := make(chan []string)
	sizes := map[string]int64{}
	for _, e := range b.Manifest {
		sizes[e.Sha] = e.Size
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batches {
				data, _ := json.Marshal(batch)
				resp, err := r.do(ctx, "POST", "/v1/runner/cas", bytes.NewReader(data))
				if err == nil {
					if resp.StatusCode != 200 {
						err = hubError(resp)
					} else {
						_, err = workspace.ReadBlobs(resp.Body, false, limit, filepath.Join(r.sessions.cfg.DataDir, "cas"), nil)
					}
					resp.Body.Close()
				}
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
send:
	for len(missing) > 0 {
		n := 1
		total := sizes[missing[0]]
		for n < min(256, len(missing)) && total+sizes[missing[n]] <= limit {
			total += sizes[missing[n]]
			n++
		}
		select {
		case batches <- missing[:n]:
			missing = missing[n:]
		case <-ctx.Done():
			break send
		}
	}
	close(batches)
	wg.Wait()
	if first != nil {
		return first
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (r *Runner) executeBazel(rj *runnerJob) (procResult, api.RunnerDone) {
	var done api.RunnerDone
	spec := rj.spec
	b := spec.Bazel
	s := r.sessions
	defer func() {
		s.mu.Lock()
		delete(s.jobs, spec.ID)
		s.mu.Unlock()
		os.RemoveAll(s.jobDir(spec.ID))
	}()
	deadline := time.Now().Add(time.Duration(spec.TimeoutSec) * time.Second)
	if err := workspace.CheckManifest(b.Command.Manifest, spec.MaxWorkspace); err != nil {
		return failed("%v", err), done
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
		r.mu.Lock()
		state := rj.killState
		r.mu.Unlock()
		if state != "" {
			return procResult{State: state}, done
		}
		if spec.TimeoutSec > 0 && time.Now().After(deadline) {
			return procResult{State: api.Timeout}, done
		}
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
	sess.diskCacheMax = b.DiskCacheMax
	if sess.credential == "" {
		sess.credential = b.CacheCredential
		sess.cacheReadOnly = b.CacheReadOnly
		sess.idleSeconds = b.SessionIdleSec
	}
	remaining := spec.TimeoutSec
	if remaining > 0 {
		remaining = max(1, int(time.Until(deadline).Seconds()+0.999))
	}
	j := &job{Job: api.Job{ID: spec.ID, Client: b.Session.Client, SessionID: b.Session.ID, Cwd: spec.Cwd, TimeoutSec: remaining, State: api.Queued}, bz: &b.Command, runnerMaxLog: &spec.MaxLog, runnerMaxUpload: b.MaxUpload, done: make(chan struct{})}
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
	done = api.RunnerDone{SessionReady: sess.sb != nil, State: j.State, ExitCode: j.ExitCode, Signal: j.Signal, Error: j.Error, Summary: j.Summary, OutputsError: j.OutError}
	v := sess.Session
	done.Session = &v
	if err := r.retry(time.Minute, func() error {
		f, err := os.Open(filepath.Join(s.jobDir(j.ID), outputsFile))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		resp, err := r.do(r.ctx, "PUT", "/v1/runner/jobs/"+spec.ID+"/outputs", f)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return hubError(resp)
		}
		return nil
	}); err != nil {
		done.OutputsError = err.Error()
	}
	data, _ := os.ReadFile(filepath.Join(s.jobDir(j.ID), "bep.json"))
	if json.Valid(data) {
		done.BEP = data
	}
	r.retry(time.Minute, func() error { return r.postJSON(r.ctx, "/v1/runner/jobs/"+spec.ID+"/exited", done, nil) })
	if err := r.retry(time.Minute, func() error { return r.shipSessionLogs(spec.ID, &sent) }); err != nil {
		return procResult{State: "crash"}, done
	}
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
