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
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
