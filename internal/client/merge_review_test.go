package client

import (
	"strings"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/server"
)

// feat/gc x feat/artifacts: after retention deletes work/, GET /files opens a Root on a
// missing dir. It should say the work dir was deleted (job.work_deleted, retention), not 500.
func TestReviewFilesAfterWorkDeleted(t *testing.T) {
	ts := startServer(t, server.Config{KeepWork: 100 * time.Millisecond, GCInterval: 50 * time.Millisecond})
	id := ts.detach("", "-c", "echo hi > out.txt")
	ts.waitDone(id)
	ts.waitFor(id, func(j api.Job) bool { return j.WorkDeleted })
	r := ts.tun(t.TempDir(), "get", id, "out.txt")
	if strings.Contains(r.stderr, "HTTP 500") || !strings.Contains(r.stderr, "keep-work") {
		t.Errorf("get after work deleted: code %d stderr %q (want a 410/404 that mentions --keep-work)", r.code, r.stderr)
	}
}
