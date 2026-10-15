package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

// Review round 2: tun serve --ephemeral removes its data dir with os.RemoveAll, which fails
// (silently) on read-only dirs a job left behind (chmod, or Go's read-only module cache), so
// the "fresh temporary data directory, removed on shutdown" is leaked. The rest of the server
// uses workspace.RemoveAll for exactly this reason.
func TestReview2EphemeralRemovesReadOnlyDirs(t *testing.T) {
	stdin, input := io.Pipe()
	output, stdout := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- serve([]string{"--ephemeral", "--exit-on-eof", "--sandbox", "none"}, stdin, stdout, &stderr)
		stdout.Close()
	}()
	defer input.Close()
	lines := bufio.NewReader(output)
	urlLine, _ := lines.ReadString('\n')
	tokenLine, _ := lines.ReadString('\n')
	base := strings.TrimSpace(strings.TrimPrefix(urlLine, "TUN_URL="))
	token := strings.TrimSpace(strings.TrimPrefix(tokenLine, "TUN_TOKEN="))
	do := func(method, path, body string) api.Job {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("X-Tun-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var j api.Job
		if err := json.NewDecoder(resp.Body).Decode(&j); err != nil || resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, err)
		}
		return j
	}
	j := do("POST", "/v1/jobs", `{"script": "mkdir -p ro && touch ro/f && chmod 555 ro"}`)
	for deadline := time.Now().Add(20 * time.Second); !api.Terminal(j.State); j = do("GET", "/v1/jobs/"+j.ID, "") {
		if time.Now().After(deadline) {
			t.Fatalf("job stuck: %+v", j)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if j.State != api.Exited || j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("job: %+v", j)
	}
	input.Close()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("EOF did not stop server")
	}
	diagnostic := stderr.String()
	start := strings.Index(diagnostic, "(data ")
	end := strings.Index(diagnostic, ", sandbox")
	if start < 0 || end < start {
		t.Fatalf("missing data diagnostic: %s", diagnostic)
	}
	dir := diagnostic[start+6 : end]
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		workspace.RemoveAll(dir)
		t.Fatalf("temporary data dir %s remains after shutdown (stat: %v)", dir, err)
	}
}
