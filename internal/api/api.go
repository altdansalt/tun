// Package api holds the JSON types shared by the tun server and client.
package api

import "time"

// Job states.
const (
	Queued   = "queued"
	Running  = "running"
	Exited   = "exited"
	Failed   = "failed" // couldn't start (or server restarted under it)
	Timeout  = "timeout"
	Canceled = "canceled"
)

// States lists every job state.
var States = []string{Queued, Running, Exited, Failed, Timeout, Canceled}

// MaxTimeoutSec is the largest timeout_sec the server accepts (7 days).
const MaxTimeoutSec = 7 * 24 * 60 * 60

// Terminal reports whether state is final.
func Terminal(state string) bool {
	return state != Queued && state != Running
}

// CreateJob is the body of POST /v1/jobs. Exactly one of Argv or Script is set.
type CreateJob struct {
	Argv       []string          `json:"argv,omitempty"`
	Script     string            `json:"script,omitempty"`
	Cwd        string            `json:"cwd,omitempty"` // relative to the workspace root
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
	Workspace  string            `json:"workspace,omitempty"` // sha256 of an uploaded tar.gz blob; empty = empty dir
	Service    bool              `json:"service,omitempty"`   // long-running service: see service.go
	Ports      []int             `json:"ports,omitempty"`     // ports to expose (service only)
	// Outputs are the paths (files, dirs, globs) the client will fetch when the job ends
	// (tun run --out). A runner uploads exactly these; see Job.Runner.
	Outputs []string `json:"outputs,omitempty"`
	// Bazel makes this a tun bazel job (Argv, Script and Workspace must be empty): sync the
	// manifest into the session's workspace, then run bazel there. See BazelJob.
	Bazel *BazelJob `json:"bazel,omitempty"`
}

// Job is the job object returned by the API.
type Job struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	ExitCode   *int       `json:"exit_code"`
	Signal     *int       `json:"signal"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	Argv       []string   `json:"argv,omitempty"`
	Script     string     `json:"script,omitempty"`
	Cwd        string     `json:"cwd"`
	TimeoutSec int        `json:"timeout_sec"`
	Workspace  string     `json:"workspace,omitempty"`
	Sandbox    string     `json:"sandbox"`
	Error      string     `json:"error,omitempty"`
	// WorkDeleted: retention deleted the job's work/home/tmp dirs; status and logs remain.
	WorkDeleted bool   `json:"work_deleted,omitempty"`
	Service     bool   `json:"service,omitempty"`
	Ports       []Port `json:"ports,omitempty"`
	// Client is who submitted the job: the exe.dev proxy's X-Exedev-Source-Vm header, else
	// the client's X-Tun-Client header (its hostname), else the remote address.
	Client string `json:"client,omitempty"`
	// Runner is where the job runs or ran: "local" (the hub itself) or a runner's name. Set
	// when the job leaves the queue. Files of a runner job are only its declared Outputs.
	Runner  string   `json:"runner,omitempty"`
	Outputs []string `json:"outputs,omitempty"`
	// Session (name) and SessionID: the warm Bazel session a tun bazel job runs in.
	Session   string `json:"session,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	// Computed each time the job is served. QueueSec is the time from creation to
	// start (or to now/end if it never started); RunSec the time from start to end (or now).
	QueueSec float64 `json:"queue_sec"`
	RunSec   float64 `json:"run_sec"`
	// QueuePosition is set only while the job waits in the FIFO: 1 = next to start.
	QueuePosition *int `json:"queue_position,omitempty"`
}

// Health is the body of GET /v1/healthz: server config and current load.
type Health struct {
	OK      bool   `json:"ok"`
	Sandbox string `json:"sandbox"`
	// NetworkIsolation: each job has its own network namespace (egress via an HTTP proxy;
	// services reachable only through the server's proxy). False: jobs share the host network.
	NetworkIsolation bool         `json:"network_isolation"`
	MaxJobs          int          `json:"max_jobs"` // job slots: local_slots plus every live runner's slots
	MaxServices      int          `json:"max_services"`
	Running          int          `json:"running"`  // non-service jobs running (at most max_jobs)
	Queued           int          `json:"queued"`   // jobs waiting for a slot
	Services         int          `json:"services"` // service jobs running (at most max_services)
	LocalSlots       int          `json:"local_slots"`
	Runners          []RunnerInfo `json:"runners"`
	Sessions         int          `json:"sessions"` // tun bazel sessions (see GET /v1/sessions)
	MaxSessions      int          `json:"max_sessions"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// Manifest entry types (tun bazel's incremental sync).
const (
	EntryFile = "f"
	EntryDir  = "d"
	EntryLink = "l"
)

// ManifestEntry is one path of a workspace manifest: a regular file (content digest, size,
// permission bits), a directory (permission bits) or a symlink (target). Paths are relative,
// slash-separated, sorted with each directory right before its contents.
type ManifestEntry struct {
	Path   string `json:"p"`
	Type   string `json:"t"`
	Mode   uint32 `json:"m,omitempty"`
	Sha    string `json:"s,omitempty"`
	Size   int64  `json:"n,omitempty"`
	Target string `json:"l,omitempty"`
}

// BazelJob is CreateJob.Bazel: run `bazel <Args...>` in a warm session (tun bazel).
type BazelJob struct {
	Session string `json:"session"` // session name; empty = "default"
	Cold    bool   `json:"cold,omitempty"`
	// Root identifies the client's workspace (its absolute path on the client); together
	// with the client name and Session it selects the session.
	Root     string          `json:"root"`
	Manifest []ManifestEntry `json:"manifest"`
	Args     []string        `json:"args"` // bazel command and its args, passed through
}

// MissingBlobs is the body of the 409 answer to a bazel job whose manifest names file
// contents the server doesn't have: upload them (POST /v1/cas) and create the job again.
type MissingBlobs struct {
	Error   string   `json:"error"`
	Missing []string `json:"missing"`
}

// Session is a warm Bazel session (GET /v1/sessions, tun sessions).
type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Client    string    `json:"client"`
	Root      string    `json:"root"` // the client's workspace path
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
	Commands  int       `json:"commands"` // bazel commands run in it
	LastJob   string    `json:"last_job,omitempty"`
	// Computed when served.
	State     string    `json:"state"`         // idle, busy (running a command), stopping
	Job       string    `json:"job,omitempty"` // the running job, if busy
	Server    bool      `json:"bazel_server"`  // a Bazel server (session sandbox) is up
	ExpiresAt time.Time `json:"expires_at"`
}
