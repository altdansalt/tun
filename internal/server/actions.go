package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/reapi"
)

// executeAction joins the existing FIFO, sharing slots with v0 jobs.
func (s *Server) executeAction(ctx context.Context, action, command []byte) ([]byte, error) {
	j := &job{actionCtx: ctx, done: make(chan struct{}), Action: action, Command: command, Job: api.Job{ID: newID(), State: api.Queued, CreatedAt: time.Now().UTC(), TimeoutSec: 86400, Sandbox: s.cfg.Sandbox}}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("server shutting down")
	}
	s.jobs[j.ID] = j
	s.queue = append(s.queue, j)
	s.schedule()
	s.mu.Unlock()
	select {
	case <-j.done:
	case <-ctx.Done():
		s.mu.Lock()
		j.killReq, j.killReqAt = api.Canceled, time.Now()
		for i, q := range s.queue {
			if q == j {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.finish(j, api.Canceled, nil, nil, "canceled while queued")
				break
			}
		}
		if j.rn != nil {
			j.rn.poke()
		}
		if j.kill != nil {
			j.kill(api.Canceled)
		} else {
			j.cancelReq = true
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, j.ID)
	if j.Sandbox != "bwrap" {
		reapi.DisableActionCache(ctx)
	}
	if j.State != api.Exited {
		return nil, &reapi.ExecutionError{Code: j.ActionStatus, Message: j.Error, Missing: j.ActionMissing}
	}
	return j.ActionResult, nil
}
func (s *Server) runAction(j *job) {
	ctx, cancel := context.WithCancel(j.actionCtx)
	defer cancel()
	s.mu.Lock()
	if s.closed || j.cancelReq {
		s.finish(j, api.Canceled, nil, nil, "canceled before execution")
		s.mu.Unlock()
		return
	}
	reapi.MarkExecuting(ctx)
	now := time.Now().UTC()
	j.State, j.StartedAt = api.Running, &now
	j.kill = func(string) { cancel() }
	s.mu.Unlock()
	result, err := reapi.LocalExecutor(s.actionStore, s.cfg.DataDir, s.cfg.Sandbox)(ctx, j.Action, j.Command)
	s.mu.Lock()
	defer s.mu.Unlock()
	j.ActionResult = result
	if err != nil {
		j.ActionStatus = actionStatus(err)
		var ex *reapi.ExecutionError
		if errors.As(err, &ex) {
			j.ActionMissing = ex.Missing
		}
		s.finish(j, api.Failed, nil, nil, err.Error())
	} else {
		code := 0
		s.finish(j, api.Exited, &code, nil, "")
	}
}

type casTransfer struct {
	Digest, Data []byte
	Upload       bool
}

func (s *Server) runnerCAS(w http.ResponseWriter, r *http.Request) {
	if s.actionStore == nil {
		writeErr(w, 503, "REAPI not enabled")
		return
	}
	var req casTransfer
	if err := json.NewDecoder(io.LimitReader(r.Body, 128<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if reapi.BlobSize(req.Digest) > reapi.MaxExecutorBlob || len(req.Data) > reapi.MaxExecutorBlob {
		writeErr(w, 413, "runner CAS blob exceeds 96MiB limit")
		return
	}
	var err error
	if req.Upload {
		err = s.actionStore.PutBlob(req.Digest, req.Data)
	} else {
		req.Data, err = s.actionStore.ReadBlob(req.Digest)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, 404, err.Error())
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, req)
}
func (r *Runner) executeAction(j *runnerJob) (procResult, api.RunnerDone) {
	var done api.RunnerDone
	r.reclaimBlobs() // also check before action input materialization, not only polls
	r.mu.Lock()
	store := r.actionStore
	var err error
	if store == nil {
		store, err = reapi.Open(filepath.Join(r.cfg.DataDir, "reapi-cas"), 10<<30)
		r.actionStore = store
	}
	ctx, cancel := context.WithCancel(r.ctx)
	j.actionCancel = cancel
	if j.killState != "" {
		cancel()
	}
	r.mu.Unlock()
	defer cancel()
	if err != nil {
		return failed("CAS: %v", err), done
	}
	if r.cfg.MinFree.set() {
		store.Reclaim(func() bool { d := diskInfo(r.cfg.DataDir, r.cfg.MinFree); return d != nil && d.Low })
	}
	remote := &reapi.RemoteStore{Local: store, Fetch: func(d []byte) ([]byte, error) {
		var out casTransfer
		err := r.postJSON(ctx, "/v1/runner/cas", casTransfer{Digest: d}, &out)
		return out.Data, err
	}, Upload: func(d, b []byte) error {
		return r.postJSON(ctx, "/v1/runner/cas", casTransfer{Digest: d, Data: b, Upload: true}, nil)
	}}
	if err = r.postJSON(ctx, "/v1/runner/jobs/"+j.spec.ID+"/started", struct{}{}, nil); err != nil {
		return procResult{State: "crash"}, done
	}
	result, err := reapi.LocalExecutor(remote, r.cfg.DataDir, r.cfg.Sandbox)(ctx, j.spec.Action, j.spec.Command)
	if err != nil {
		done.ActionStatus = actionStatus(err)
		var ex *reapi.ExecutionError
		if errors.As(err, &ex) {
			done.ActionMissing = ex.Missing
		}
		return failed("executing action: %v", err), done
	}
	done.ActionResult = result
	code := 0
	return procResult{State: api.Exited, Code: &code}, done
}

func actionStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return 4
	}
	if errors.Is(err, context.Canceled) {
		return 1
	}
	var ex *reapi.ExecutionError
	if errors.As(err, &ex) {
		return ex.Code
	}
	return 13
}
