package client

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/server"
)

// Services skip the FIFO but are created in state "queued" until run() takes s.mu and sets
// "running". cancelJob on a "queued" job calls finish() directly (the queue-removal path), so
// a cancel in that window finishes the job (closing j.done, releasing the slot) while run()
// goes on to start the process and later finishes it a second time: close of closed channel
// (server panic) and svc.running driven negative / the job resurrected as "running".
func TestReviewServiceCancelRace(t *testing.T) {
	ts := startServer(t, server.Config{MaxServices: 1})
	c := ts.client()
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		j, err := c.Create(ctx, api.CreateJob{Argv: []string{"sleep", "30"}, Service: true})
		if err != nil {
			t.Fatalf("iteration %d: create: %v", i, err)
		}
		cj, err := c.Cancel(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The cancel was acknowledged; the job must end up (and stay) canceled promptly.
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			cur := ts.status(j.ID)
			if cur.State == api.Canceled && time.Now().After(deadline.Add(-2500*time.Millisecond)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: cancel returned state %q but the job is %q after 3s (process resurrected after cancel; slot already released)", i, cj.State, cur.State)
			}
		}
	}
}

// The proxy is the only authenticated way to reach a service, but services listen on the
// host's 127.0.0.1 and jobs share the host network, so any job (any user's) can talk to any
// service directly, with no token. Documents the gap against "behind the usual bearer auth".
func TestReviewServiceReachableFromOtherJobsWithoutToken(t *testing.T) {
	ts := startServer(t, server.Config{})
	id, _ := ts.startService("--port", "8080")
	hp := ts.status(id).Ports[0].HostPort
	r := ts.tun("", "run", "--no-workspace", "-e", "P="+itoa(hp), "-c",
		`exec 3<>/dev/tcp/127.0.0.1/$P; printf 'GET /secret HTTP/1.0\r\n\r\n' >&3; cat <&3`)
	if r.code == 0 && len(r.stdout) > 0 {
		t.Errorf("an unrelated job reached service %s on host port %d without a token: %q", id, hp, r.stdout)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
