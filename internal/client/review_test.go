package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// These are contract assertions: failures reproduce review findings on main.
func TestReviewLogSymlinkIsolation(t *testing.T) {
	for _, mode := range []string{sandbox.None, sandbox.Bwrap} {
		t.Run(mode, func(t *testing.T) {
			if mode == sandbox.Bwrap && !sandbox.BwrapUsable() {
				t.Skip("bwrap unusable")
			}
			ts := startServer(t, server.Config{Sandbox: mode})
			id := ts.detach("", "--no-workspace", "-c", `rm ../stdout; ln -s ../../token ../stdout`)
			j := ts.waitDone(id)
			if j.ExitCode == nil || *j.ExitCode != 0 {
				t.Fatalf("setup: %+v", j)
			}
			var out bytes.Buffer
			_, err := ts.client().Logs(context.Background(), id, "stdout", 0, false, &out)
			if strings.Contains(out.String(), ts.srv.Token()) {
				t.Fatal("logs API disclosed server bearer token through job-created symlink")
			}
			if err == nil && out.Len() != 0 {
				t.Fatalf("unexpected log contents: %q", out.String())
			}
		})
	}
}

func TestReviewMalformedHTTP(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "--no-workspace", "-c", "printf abc")
	ts.waitDone(id)
	cases := []struct{ name, method, path, body string }{
		{"trailing JSON", "POST", "/v1/jobs", `{"script":"true"} garbage`},
		{"fractional limit", "GET", "/v1/jobs?limit=1.5", ""},
		{"junk offset", "GET", "/v1/jobs/" + id + "/stdout?offset=1junk", ""},
		{"overflow timeout", "POST", "/v1/jobs", `{"script":"sleep 1","timeout_sec":9223372036854775807}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.url+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+ts.srv.Token())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 400 {
				t.Errorf("status=%d body=%s; want 400", resp.StatusCode, b)
			}
		})
	}
}

func TestReviewTimeoutOverflow(t *testing.T) {
	ts := startServer(t, server.Config{})
	j, err := ts.client().Create(context.Background(), api.CreateJob{Script: "sleep 1", TimeoutSec: 9223372036854775807})
	if err != nil {
		return
	} // Rejecting an unrepresentable duration is also correct.
	j = ts.waitDone(j.ID)
	if j.State == api.Timeout {
		t.Fatalf("huge positive timeout expired immediately: state=%s duration=%v", j.State, j.EndedAt.Sub(*j.StartedAt))
	}
}

func TestReviewLogOffsetsAtCompletion(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "--no-workspace", "-c", `printf abc; sleep 0.1; printf def; printf err >&2`)
	for _, offset := range []int64{0, 3, 6, 100} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out bytes.Buffer
			_, err := ts.client().Logs(ctx, id, "stdout", offset, true, &out)
			want := ""
			if offset < 6 {
				want = "abcdef"[offset:]
			}
			if err != nil || out.String() != want {
				t.Fatalf("output=%q err=%v want=%q", out.String(), err, want)
			}
		})
	}
}

func TestReviewPersistenceSymlinkIsolation(t *testing.T) {
	ts := bwrapServer(t)
	target := filepath.Join(ts.data, "server-owned")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	id := ts.detach("", "--no-workspace", "-c", fmt.Sprintf("ln -s %q ../job.json.tmp", target))
	ts.waitDone(id)
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "unchanged" {
		t.Fatalf("server-owned file overwritten by persistence: %s", b)
	}
}
