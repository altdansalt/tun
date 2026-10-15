package client

import (
	"context"
	"strconv"
	"strings"
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
	var ids []string
	for i := 0; i < 300; i++ {
		j, err := c.Create(ctx, api.CreateJob{Argv: []string{"sleep", "30"}, Service: true})
		if err != nil {
			t.Fatalf("iteration %d: create: %v", i, err)
		}
		if _, err := c.Cancel(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
		if cur := ts.waitDone(j.ID); cur.State != api.Canceled {
			t.Fatalf("iteration %d: %+v", i, cur)
		}
		ids = append(ids, j.ID)
	}
	// None may be resurrected afterwards (run() starting the process after the cancel), and
	// the single slot must be free (no double release / leak).
	time.Sleep(500 * time.Millisecond)
	for _, id := range ids {
		if cur := ts.status(id); cur.State != api.Canceled {
			t.Fatalf("job %s resurrected after cancel: %+v", id, cur)
		}
	}
	id, _ := ts.startService()
	ts.tun("", "cancel", id)
}

// The proxy is the only authenticated way to reach a service, but services listen on the
// host's 127.0.0.1 and jobs share the host network, so any job (any user's) can talk to any
// service directly, with no token. This is a documented known limitation (SPEC.md Services,
// tun run -h); the test pins that behavior so a fix (per-job netns) flips it deliberately.
func TestReviewServiceReachableFromOtherJobsWithoutToken(t *testing.T) {
	ts := startServer(t, server.Config{})
	id, _ := ts.startService("--port", "8080")
	hp := ts.status(id).Ports[0].HostPort
	r := ts.tun("", "run", "--no-workspace", "-e", "P="+itoa(hp), "-c",
		`exec 3<>/dev/tcp/127.0.0.1/$P; printf 'GET /secret HTTP/1.0\r\n\r\n' >&3; cat <&3`)
	defer ts.tun("", "cancel", id)
	if r.code != 0 || !strings.Contains(r.stdout, "path=/secret") {
		t.Errorf("known limitation changed? an unrelated job could not reach service %s on host port %d: %+v (update SPEC.md Services and tun run -h)", id, hp, r)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
