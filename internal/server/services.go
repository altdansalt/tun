package server

// Long-running service jobs: they bypass the max-jobs FIFO (bounded instead by MaxServices, and
// rejected rather than queued when that is full), have no timeout unless one is given, get a
// free host port per requested port, and are reachable through a reverse proxy at
// /v1/jobs/{id}/ports/{port}/... behind the normal bearer auth.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// services is the Server's bookkeeping for running service jobs. Guarded by Server.mu.
type services struct {
	running   int
	hostPorts map[int]bool // host ports held by live services
}

// checkService validates the service fields of a create request.
func checkService(req *api.CreateJob) error {
	if len(req.Ports) > 0 && !req.Service {
		return fmt.Errorf("ports are only allowed for service jobs (set \"service\": true / tun run --service)")
	}
	if len(req.Ports) > api.MaxPorts {
		return fmt.Errorf("at most %d ports per service, got %d", api.MaxPorts, len(req.Ports))
	}
	seen := map[int]bool{}
	for _, p := range req.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port %d out of range 1-65535", p)
		}
		if seen[p] {
			return fmt.Errorf("port %d given twice", p)
		}
		seen[p] = true
	}
	return nil
}

// serviceSlotFree reports (as an error) whether max-services is reached. Caller holds s.mu.
func (s *Server) serviceSlotFree() error {
	if s.svc.running >= s.cfg.MaxServices {
		return fmt.Errorf("%d services already running (tun serve --max-services %d); cancel one first (tun ls shows them, \"service\": true)", s.svc.running, s.cfg.MaxServices)
	}
	return nil
}

// admitService reserves a service slot and allocates host ports for j. Caller holds s.mu.
func (s *Server) admitService(j *job, ports []int) (int, error) {
	if err := s.serviceSlotFree(); err != nil {
		return 429, err
	}
	if s.svc.hostPorts == nil {
		s.svc.hostPorts = map[int]bool{}
	}
	for _, p := range ports {
		hp, err := s.freePort()
		if err != nil {
			s.releaseService(j)
			return 500, fmt.Errorf("allocating a host port: %v", err)
		}
		s.svc.hostPorts[hp] = true
		j.Ports = append(j.Ports, api.Port{Port: p, HostPort: hp, Path: fmt.Sprintf("/v1/jobs/%s/ports/%d/", j.ID, p)})
	}
	s.svc.running++
	return 0, nil
}

// freePort asks the kernel for an unused port that no live service holds. Caller holds s.mu.
func (s *Server) freePort() (int, error) {
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if !s.svc.hostPorts[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port found")
}

// releaseService frees j's host ports. Caller holds s.mu.
func (s *Server) releaseService(j *job) {
	for _, p := range j.Ports {
		delete(s.svc.hostPorts, p.HostPort)
	}
}

// startService runs an admitted service job outside the FIFO. Caller holds s.mu.
func (s *Server) startService(j *job) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		go s.probe(j)
		s.run(j)
	}()
}

// serviceDone releases a finished service's slot and ports. Called from finish (s.mu held).
func (s *Server) serviceDone(j *job) {
	if j.Service {
		s.releaseService(j)
		s.svc.running--
	}
}

// portEnv adds TUN_PORT_<port> (and PORT, if there is exactly one) to a job's environment.
func portEnv(j *job, m map[string]string) {
	for _, p := range j.Ports {
		m["TUN_PORT_"+strconv.Itoa(p.Port)] = strconv.Itoa(p.HostPort)
	}
	if len(j.Ports) == 1 {
		m["PORT"] = strconv.Itoa(j.Ports[0].HostPort)
	}
}

// probe marks each port ready once something accepts connections on its host port.
func (s *Server) probe(j *job) {
	for {
		s.mu.Lock()
		ports, closed := j.Ports, s.closed
		s.mu.Unlock()
		pending := false
		for i, p := range ports {
			if p.Ready {
				continue
			}
			c, err := s.dialService(j, p.HostPort)
			if err != nil {
				pending = true
				continue
			}
			c.Close()
			s.mu.Lock()
			// Copy on write: snapshots handed out earlier share the old slice.
			np := append([]api.Port(nil), j.Ports...)
			np[i].Ready = true
			j.Ports = np
			if !api.Terminal(j.State) {
				s.persist(j)
			}
			s.mu.Unlock()
		}
		if !pending || closed {
			return
		}
		select {
		case <-j.done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dialService connects to a running service's port: inside its network namespace, or on the
// host's loopback when jobs share the host network.
func (s *Server) dialService(j *job, port int) (net.Conn, error) {
	if !s.cfg.PrivateNet {
		return net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	}
	s.mu.Lock()
	n := j.net
	s.mu.Unlock()
	if n == nil {
		return nil, fmt.Errorf("job is not running")
	}
	return n.Dial(port)
}

// proxy serves /v1/jobs/{id}/ports/{port}/... by reverse-proxying to the service's host port.
// Streaming responses are flushed as they arrive and protocol upgrades (websockets) pass through.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	snap := s.snapshot(j)
	n, _ := strconv.Atoi(r.PathValue("port"))
	var port *api.Port
	var have []string
	for i, p := range snap.Ports {
		if p.Port == n {
			port = &snap.Ports[i]
		}
		have = append(have, strconv.Itoa(p.Port))
	}
	sort.Strings(have)
	switch {
	case port == nil:
		writeErr(w, 404, fmt.Sprintf("job %s has no port %q (its ports: [%s]; expose with tun run --service --port N)", j.ID, r.PathValue("port"), strings.Join(have, " ")))
		return
	case snap.State != api.Running:
		writeErr(w, 409, fmt.Sprintf("job %s is %s, not running; see tun logs %s --stderr", j.ID, snap.State, j.ID))
		return
	}
	prefix := fmt.Sprintf("/v1/jobs/%s/ports/%s", r.PathValue("id"), r.PathValue("port"))
	target := "127.0.0.1:" + strconv.Itoa(port.HostPort)
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return s.dialService(j, port.HostPort)
	}}
	defer tr.CloseIdleConnections()
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", target, ""
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, prefix)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path, pr.Out.URL.RawPath = "/", ""
			}
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			// The tun token is not the service's business. With X-Tun-Token, Authorization
			// is the service's own (e.g. its basic auth) and passes through unless it is ours.
			pr.Out.Header.Del("X-Tun-Token")
			authRequest := pr.In.Clone(pr.In.Context())
			authRequest.Header.Del("X-Tun-Token")
			if s.authenticate(authRequest) {
				pr.Out.Header.Del("Authorization")
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeErr(w, 502, fmt.Sprintf("job %s port %d (host port %d): %v", j.ID, n, port.HostPort, err))
		},
	}
	rp.ServeHTTP(w, r)
}
