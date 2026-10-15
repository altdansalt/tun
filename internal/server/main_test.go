package server

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEphemeral(t *testing.T) {
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
	url, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	token, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "TUN_URL=http://127.0.0.1:") || !strings.HasPrefix(token, "TUN_TOKEN=") {
		t.Fatalf("environment: %q %q", url, token)
	}
	base := strings.TrimSpace(strings.TrimPrefix(url, "TUN_URL="))
	req, _ := http.NewRequest("GET", base+"/v1/jobs", nil)
	req.Header.Set("X-Tun-Token", strings.TrimSpace(strings.TrimPrefix(token, "TUN_TOKEN=")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	input.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, &stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EOF did not stop server")
	}
	rest, _ := io.ReadAll(lines)
	if len(rest) != 0 {
		t.Fatalf("extra stdout: %q", rest)
	}
	// The diagnostic includes the temporary path, allowing cleanup to be checked.
	diagnostic := stderr.String()
	start := strings.Index(diagnostic, "(data ")
	if start < 0 {
		t.Fatalf("missing data diagnostic: %s", diagnostic)
	}
	end := strings.Index(diagnostic[start:], ", sandbox")
	if end < 0 {
		t.Fatalf("missing data diagnostic: %s", diagnostic)
	}
	dir := diagnostic[start+6 : start+end]
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary dir remains: %s: %v", dir, err)
	}
}

func TestEphemeralConflicts(t *testing.T) {
	for _, flag := range []string{"--addr", "--data"} {
		var out, diagnostic bytes.Buffer
		code := serve([]string{"--ephemeral", flag, "unused"}, strings.NewReader(""), &out, &diagnostic)
		if code != 125 || !strings.Contains(diagnostic.String(), "cannot be combined") || out.Len() != 0 {
			t.Fatalf("%s: code %d, %s", flag, code, &diagnostic)
		}
	}
}
