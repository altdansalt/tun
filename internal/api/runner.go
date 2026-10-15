package api

import "time"

// Runner protocol (see SPEC.md, Runners). Everything is runner-initiated: a runner long-polls
// the hub for work, fetches workspace blobs, and pushes logs, outputs and the end state.

// LocalRunner is the Job.Runner value of jobs run by the hub itself.
const LocalRunner = "local"

// HeaderRunner names the runner on every /v1/runner/jobs/... request.
const HeaderRunner = "X-Tun-Runner"

// MaxOutputs is the most --out paths one job may declare.
const MaxOutputs = 256

// RunnerPoll is the body of POST /v1/runner/poll. It doubles as the runner's heartbeat.
type RunnerPoll struct {
	Name     string `json:"name"`
	Instance string `json:"instance"` // random per runner process: tells a restart from a name clash
	// Seq increases with every poll of an instance; the hub ignores a poll older than one it
	// has seen (a request delayed in the network must not undo newer state). 0: unsequenced.
	Seq     int64    `json:"seq,omitempty"`
	Slots   int      `json:"slots"`   // jobs the runner runs at once
	Running []string `json:"running"` // job ids the runner holds (received and not yet reported done)
	Sandbox string   `json:"sandbox"` // the runner's sandbox mode (bwrap|none)
}

// RunnerPollResponse is the hub's answer: jobs to start and job ids to kill and drop.
type RunnerPollResponse struct {
	Jobs   []RunnerJob `json:"jobs"`
	Cancel []string    `json:"cancel"`
}

// RunnerJob is one job assignment: everything the runner needs to run it.
type RunnerJob struct {
	ID         string            `json:"id"`
	Argv       []string          `json:"argv,omitempty"`
	Script     string            `json:"script,omitempty"`
	Cwd        string            `json:"cwd"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec"`
	Workspace  string            `json:"workspace,omitempty"` // blob sha; GET /v1/blobs/{sha}
	Outputs    []string          `json:"outputs,omitempty"`   // paths to upload when the job ends
	// The hub's limits, applied by the runner.
	MaxLog       int64 `json:"max_log"`
	MaxWorkspace int64 `json:"max_workspace"`
	MaxDownload  int64 `json:"max_download"`
}

// LogAck is the body of a 2xx (or 409 offset mismatch) answer to a log chunk: the hub's
// size of that log, i.e. where the runner's next chunk starts.
type LogAck struct {
	Offset int64  `json:"offset"`
	Error  string `json:"error,omitempty"`
}

// RunnerDone is the body of POST /v1/runner/jobs/{id}/done.
type RunnerDone struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Signal   *int   `json:"signal"`
	Error    string `json:"error,omitempty"`
	// Outcome of collecting the declared outputs (uploaded before done).
	OutputsMissing []string `json:"outputs_missing,omitempty"`
	OutputsSkipped []string `json:"outputs_skipped,omitempty"`
	OutputsError   string   `json:"outputs_error,omitempty"`
}

// RunnerInfo describes a live runner in GET /v1/healthz.
type RunnerInfo struct {
	Name     string    `json:"name"`
	Slots    int       `json:"slots"`
	Running  int       `json:"running"` // jobs assigned to it and not yet ended
	LastSeen time.Time `json:"last_seen"`
	Sandbox  string    `json:"sandbox,omitempty"`
	Source   string    `json:"source,omitempty"` // who connected (exe.dev source VM, else IP)
}
