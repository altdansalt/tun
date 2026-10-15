package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/server"
)

// The job dir is job-writable, so a job can make it read-only. The sweep's removeAll only
// chmods directories *below* the dir it deletes (work/, home/, tmp/), never the job dir
// itself, so unlinking work/ from a 0500 job dir fails on every sweep and the disk is never
// freed. (It also makes the final persist of job.json fail.)
func TestReviewRetentionJobDirReadOnly(t *testing.T) {
	ts := startServer(t, server.Config{KeepWork: 200 * time.Millisecond, GCInterval: 50 * time.Millisecond})
	id := ts.detach("", "-c", "echo data > big; chmod 500 ..")
	ts.waitDone(id)
	t.Cleanup(func() { os.Chmod(filepath.Join(ts.data, "jobs", id), 0o700) }) // let t.TempDir cleanup succeed
	time.Sleep(1500 * time.Millisecond)
	if !ts.status(id).WorkDeleted || exists(filepath.Join(ts.data, "jobs", id, "work")) {
		j := ts.status(id)
		t.Errorf("work dir of a job that chmod'd its job dir was never deleted: work_deleted=%v state=%s err=%q", j.WorkDeleted, j.State, j.Error)
	}
}
