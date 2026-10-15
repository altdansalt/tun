package server

// Services use runner-initiated upgraded HTTP connections. The hub never needs a
// route to a runner; each connection is bound to an assigned job and declared port.
import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

type serviceTunnel struct {
	spec api.ServiceTunnel
	sent bool
	conn chan net.Conn
}
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (s *Server) dialRunnerService(j *job, port int) (net.Conn, error) {
	s.mu.Lock()
	rn := j.rn
	if rn == nil || api.Terminal(j.State) || len(rn.tunnels) >= 128 {
		s.mu.Unlock()
		return nil, fmt.Errorf("runner service unavailable")
	}
	t := &serviceTunnel{spec: api.ServiceTunnel{ID: randHex(16), Job: j.ID, Port: port}, conn: make(chan net.Conn)}
	if rn.tunnels == nil {
		rn.tunnels = map[string]*serviceTunnel{}
	}
	rn.tunnels[t.spec.ID] = t
	rn.poke()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(rn.tunnels, t.spec.ID); s.mu.Unlock() }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case c := <-t.conn:
		if c == nil {
			return nil, fmt.Errorf("runner service is not listening")
		}
		return c, nil
	case <-timer.C:
		return nil, fmt.Errorf("runner service connection timed out")
	case <-j.done:
		return nil, fmt.Errorf("service ended")
	case <-s.quit:
		return nil, fmt.Errorf("hub stopped")
	}
}

func (s *Server) acceptServiceTunnel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.runnerJob(w, r, true)
	if j == nil {
		s.mu.Unlock()
		return
	}
	t := j.rn.tunnels[r.PathValue("tunnel")]
	if t == nil || t.spec.Job != j.ID {
		s.mu.Unlock()
		writeErr(w, 410, "expired tunnel")
		return
	}
	s.mu.Unlock()
	if r.URL.Query().Get("failed") == "1" {
		select {
		case t.conn <- nil:
		case <-r.Context().Done():
		}
		writeJSON(w, 200, struct{}{})
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeErr(w, 400, "service tunnels require HTTP/1.1")
		return
	}
	c, b, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err = b.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tun-service\r\n\r\n"); err == nil {
		err = b.Flush()
	}
	if err != nil {
		c.Close()
		return
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case t.conn <- &bufferedConn{Conn: c, reader: b.Reader}:
	case <-timer.C:
		c.Close()
	case <-j.done:
		c.Close()
	case <-r.Context().Done():
		c.Close()
	}
}

func (r *Runner) connectServiceTunnel(t api.ServiceTunnel) {
	r.mu.Lock()
	j := r.jobs[t.Job]
	if j == nil || j.proc == nil {
		r.mu.Unlock()
		r.failServiceTunnel(t)
		return
	}
	p := j.proc
	allowed := false
	for _, port := range j.spec.Ports {
		if port.HostPort == t.Port {
			allowed = true
		}
	}
	r.mu.Unlock()
	if !allowed {
		return
	}
	var c net.Conn
	var err error
	if p.net != nil {
		c, err = p.net.Dial(t.Port)
	} else {
		c, err = net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(t.Port), time.Second)
	}
	if err != nil {
		r.failServiceTunnel(t)
		return
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	resp, err := r.do(ctx, "GET", "/v1/runner/jobs/"+t.Job+"/tunnels/"+t.ID, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	stream, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != 101 || !ok {
		return
	}
	done := make(chan struct{})
	go func() { io.Copy(stream, c); stream.Close(); close(done) }()
	io.Copy(c, stream)
	c.Close()
	<-done
}

func (rn *runnerState) pendingTunnels() bool {
	for _, t := range rn.tunnels {
		if !t.sent {
			return true
		}
	}
	return false
}

func (r *Runner) failServiceTunnel(t api.ServiceTunnel) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	resp, err := r.do(ctx, "GET", "/v1/runner/jobs/"+t.Job+"/tunnels/"+t.ID+"?failed=1", nil)
	if err == nil {
		resp.Body.Close()
	}
}
