// Package bep summarizes Bazel's newline-delimited JSON Build Event Protocol.
// It does not run Bazel or fetch artifacts. Unknown event fields are ignored.
package bep

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

type File struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
}
type Attempt struct {
	Run      int     `json:"run"`
	Shard    int     `json:"shard"`
	Attempt  int     `json:"attempt"`
	Status   string  `json:"status"`
	Duration float64 `json:"duration_sec"`
	Logs     []File  `json:"logs,omitempty"`
}
type Test struct {
	Label    string    `json:"label"`
	Status   string    `json:"status"`
	Attempts []Attempt `json:"attempts,omitempty"`
	Duration float64   `json:"duration_sec"`
	Shards   int       `json:"shards"`
}
type Target struct {
	Label   string `json:"label"`
	Success bool   `json:"success"`
	Outputs []File `json:"outputs,omitempty"`
}
type Action struct {
	Label   string `json:"label,omitempty"`
	Message string `json:"message,omitempty"`
	Logs    []File `json:"logs,omitempty"`
}

// Summary is a complete snapshot; JSON() provides a bounded representation for agents.
// ExitCode is nil until a buildFinished event or SetResult arrives.
type Summary struct {
	Command       string   `json:"command"`
	ExitCode      *int     `json:"exit_code"`
	ExitName      string   `json:"exit_name,omitempty"`
	Success       bool     `json:"success"`
	Complete      bool     `json:"complete"`
	Duration      float64  `json:"duration_sec"`
	Targets       []Target `json:"targets,omitempty"`
	Tests         []Test   `json:"tests,omitempty"`
	FailedActions []Action `json:"failed_actions,omitempty"`
}

// Parser accepts arbitrary chunks, retaining an unterminated final line until Finish.
// It is not safe for concurrent use. Snapshot results do not alias its state.
type Parser struct {
	startMillis float64
	pending     []byte
	s           Summary
	targets     map[string]Target
	tests       map[string]Test
	sets        map[string]fileSet
	groups      map[string][]string
}
type fileSet struct {
	Files    []File
	Children []string
}

func New(command string) *Parser {
	return &Parser{s: Summary{Command: command}, targets: map[string]Target{}, tests: map[string]Test{}, sets: map[string]fileSet{}, groups: map[string][]string{}}
}
func (p *Parser) Write(b []byte) (int, error) {
	p.pending = append(p.pending, b...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			break
		}
		line := p.pending[:i]
		p.pending = p.pending[i+1:]
		if err := p.event(line); err != nil {
			return len(b), err
		}
	}
	return len(b), nil
}

// Finish consumes a final line without a newline. Truncated JSON is an error;
// Snapshot still returns all preceding events after any parse error.
func (p *Parser) Finish() error { b := p.pending; p.pending = nil; return p.event(b) }

// SetResult supplies the actual process result (also for query/empty or partial BEP).
// Call after ingestion; process exit status is authoritative.
func (p *Parser) SetResult(code int, elapsed time.Duration) {
	if p.s.ExitCode != nil && *p.s.ExitCode != code {
		p.s.ExitName = ""
	}
	if code == 0 && p.s.ExitName == "" {
		p.s.ExitName = "SUCCESS"
	}
	p.s.ExitCode = &code
	p.s.Success = code == 0
	p.s.Complete = true
	p.s.Duration = elapsed.Seconds()
}

type rawFile struct {
	Name       string   `json:"name"`
	URI        string   `json:"uri"`
	PathPrefix []string `json:"pathPrefix"`
}

func files(fs []rawFile, output bool) []File {
	var r []File
	for _, f := range fs {
		u, e := url.Parse(f.URI)
		// An output's workspace path comes from its prefix and name, also when the URI
		// points to a remote cache (bytestream://).
		if e != nil || u.Scheme != "file" && !(output && u.Scheme == "bytestream") {
			continue
		}
		name := u.Path
		if output {
			name = path.Join(append(f.PathPrefix, f.Name)...)
			if i := strings.Index(name, "bazel-out/"); i >= 0 {
				name = name[i:]
			}
			parts := strings.SplitN(name, "/", 4)
			if len(parts) == 4 && parts[0] == "bazel-out" && parts[2] == "bin" {
				name = "bazel-bin/" + parts[3]
			}
			if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
				continue
			}
		}
		r = append(r, File{f.Name, name})
	}
	return r
}
func (p *Parser) event(b []byte) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var e struct {
		ID struct {
			TargetCompleted struct{ Label string }
			TestResult      struct {
				Label               string
				Run, Shard, Attempt int
			}
			TestSummary struct{ Label string }
			NamedSet    struct{ ID string } `json:"namedSet"`
		}
		Started *struct {
			Command         string
			StartTimeMillis json.Number
		}
		Finished *struct {
			FinishTimeMillis json.Number
			OverallSuccess   bool
			ExitCode         struct {
				Name string
				Code int
			}
		}
		Completed *struct {
			Success     bool
			OutputGroup []struct {
				Name     string
				FileSets []struct{ ID string }
			}
		}
		NamedSetOfFiles *struct {
			Files    []rawFile
			FileSets []struct{ ID string }
		}
		TestResult *struct {
			Status                    string
			TestAttemptDurationMillis json.Number
			TestActionOutput          []rawFile
		}
		TestSummary *struct {
			OverallStatus          string
			TotalRunDurationMillis json.Number
			ShardCount             int
		}
		Action *struct {
			Success        bool
			Label          string
			Stderr, Stdout rawFile
			FailureDetail  struct{ Message string }
		}
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return fmt.Errorf("BEP event: %w", err)
	}
	if e.Started != nil {
		p.s.Command = e.Started.Command
		p.startMillis, _ = e.Started.StartTimeMillis.Float64()
	}
	if e.Finished != nil {
		end, _ := e.Finished.FinishTimeMillis.Float64()
		if p.startMillis > 0 && end >= p.startMillis {
			p.s.Duration = (end - p.startMillis) / 1000
		}
		c := e.Finished.ExitCode.Code
		p.s.ExitCode = &c
		p.s.ExitName = e.Finished.ExitCode.Name
		p.s.Success = e.Finished.OverallSuccess
		p.s.Complete = true
	}
	if x := e.NamedSetOfFiles; x != nil {
		s := fileSet{Files: files(x.Files, true)}
		for _, v := range x.FileSets {
			s.Children = append(s.Children, v.ID)
		}
		p.sets[e.ID.NamedSet.ID] = s
	}
	if x := e.Completed; x != nil {
		label := e.ID.TargetCompleted.Label
		p.targets[label] = Target{Label: label, Success: x.Success}
		for _, g := range x.OutputGroup {
			if g.Name == "default" {
				for _, s := range g.FileSets {
					p.groups[label] = append(p.groups[label], s.ID)
				}
			}
		}
	}
	if x := e.TestResult; x != nil {
		id := e.ID.TestResult
		t := p.tests[id.Label]
		t.Label = id.Label
		d, _ := x.TestAttemptDurationMillis.Float64()
		t.Attempts = append(t.Attempts, Attempt{id.Run, id.Shard, id.Attempt, x.Status, d / 1000, files(x.TestActionOutput, false)})
		if t.Status == "" {
			t.Status = x.Status
		}
		if id.Shard > t.Shards {
			t.Shards = id.Shard
		}
		p.tests[id.Label] = t
	}
	if x := e.TestSummary; x != nil {
		label := e.ID.TestSummary.Label
		t := p.tests[label]
		t.Label = label
		t.Status = x.OverallStatus
		d, _ := x.TotalRunDurationMillis.Float64()
		t.Duration = d / 1000
		if x.ShardCount > t.Shards {
			t.Shards = x.ShardCount
		}
		p.tests[label] = t
	}
	if x := e.Action; x != nil && !x.Success {
		p.s.FailedActions = append(p.s.FailedActions, Action{x.Label, x.FailureDetail.Message, files([]rawFile{x.Stderr, x.Stdout}, false)})
	}
	return nil
}
func (p *Parser) Snapshot() Summary {
	s := p.s
	if s.ExitCode != nil {
		c := *s.ExitCode
		s.ExitCode = &c
	}
	s.FailedActions = append([]Action(nil), s.FailedActions...)
	for i := range s.FailedActions {
		s.FailedActions[i].Logs = append([]File(nil), s.FailedActions[i].Logs...)
	}
	for label, t := range p.targets {
		seen := map[string]bool{}
		seenFile := map[string]bool{}
		var visit func(string)
		visit = func(id string) {
			if seen[id] {
				return
			}
			seen[id] = true
			set := p.sets[id]
			for _, f := range set.Files {
				if !seenFile[f.Path] {
					t.Outputs = append(t.Outputs, f)
					seenFile[f.Path] = true
				}
			}
			for _, c := range set.Children {
				visit(c)
			}
		}
		for _, id := range p.groups[label] {
			visit(id)
		}
		s.Targets = append(s.Targets, t)
	}
	for _, t := range p.tests {
		t.Attempts = append([]Attempt(nil), t.Attempts...)
		for i := range t.Attempts {
			t.Attempts[i].Logs = append([]File(nil), t.Attempts[i].Logs...)
		}
		s.Tests = append(s.Tests, t)
	}
	sort.Slice(s.Targets, func(i, j int) bool { return s.Targets[i].Label < s.Targets[j].Label })
	sort.Slice(s.Tests, func(i, j int) bool { return s.Tests[i].Label < s.Tests[j].Label })
	return s
}
