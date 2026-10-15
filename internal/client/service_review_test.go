package client

import (
	"context"
	"os"
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
	t.Parallel()
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
	// A port keeps the helper serving until canceled; without one it exits at once and
	// startService could see it end before it was ready.
	id, _ := ts.startService("--port", "8080")
	ts.tun("", "cancel", id)
}

// The proxy is the only authenticated way to reach a service. With per-job network namespaces
// a service's port exists only inside its own job's namespace, so another job can neither
// connect to it nor squat on it; the proxy still reaches the real service. (Without network
// isolation, any job can reach it directly: see SPEC.md Services.)
func TestReviewServiceNotReachableFromOtherJobs(t *testing.T) {
	t.Parallel()
	ts := netServer(t)
	id, urls := ts.startService("--port", "8080")
	defer ts.tun("", "cancel", id)
	hp := ts.status(id).Ports[0].HostPort
	r := ts.tun("", "run", "--no-workspace", "-e", "P="+itoa(hp), "-c",
		`exec 3<>/dev/tcp/127.0.0.1/$P && printf 'GET /secret HTTP/1.0\r\n\r\n' >&3 && cat <&3`)
	if r.code == 0 || strings.Contains(r.stdout, "path=/secret") {
		t.Errorf("an unrelated job reached service %s on port %d without the token: %+v", id, hp, r)
	}
	// A job listening on the same port number gets its own port, not the service's traffic.
	exe, _ := os.Executable()
	squat := ts.detach("", "--no-workspace", "-e", "TUN_TEST_SERVICE=1", "-e", "TUN_PORT_1="+itoa(hp), "--", exe, "-test.run=^TestServiceHelper$")
	ts.waitFor(squat, func(j api.Job) bool { return j.State != api.Queued })
	time.Sleep(500 * time.Millisecond)
	want := "PORT=" + itoa(hp) + " TUN_PORT_8080=" + itoa(hp)
	if code, body := ts.get(urls[8080]+"env", true); code != 200 || body != want {
		t.Errorf("proxy with a squatter on port %d: %d %q, want %q", hp, code, body, want)
	}
	if j := ts.status(squat); j.State != api.Running {
		t.Errorf("squatter could not listen on port %d in its own namespace: %+v", hp, j)
	}
	ts.tun("", "cancel", squat)
}

func itoa(n int) string { return strconv.Itoa(n) }
