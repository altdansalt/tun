package bep

import (
	"encoding/json"
	"fmt"
	"strings"
)

const ListLimit = 20

func passing(status string) bool { return status == "PASSED" || status == "FLAKY" }
func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 240 {
		return string(r[:240]) + "…"
	}
	return s
}

// Line renders a single bounded diagnostic. Failed tests take priority over targets.
func (s Summary) Line() string {
	code := "?"
	if s.ExitCode != nil {
		code = fmt.Sprint(*s.ExitCode)
	}
	prefix := fmt.Sprintf("tun: bazel %s exit=%s after %.1fs: ", short(s.Command), code, s.Duration)
	var labels []string
	for _, t := range s.Tests {
		if !passing(t.Status) {
			labels = append(labels, short(t.Label))
		}
	}
	detail := ""
	if len(labels) > 0 {
		detail = fmt.Sprintf("%d of %d tests failed", len(labels), len(s.Tests))
	} else if len(s.Tests) > 0 {
		detail = fmt.Sprintf("%d tests passed", len(s.Tests))
		flaky := 0
		for _, t := range s.Tests {
			if t.Status == "FLAKY" {
				flaky++
			}
		}
		if flaky > 0 {
			detail += fmt.Sprintf(" (%d flaky)", flaky)
		}
	} else {
		for _, t := range s.Targets {
			if !t.Success {
				labels = append(labels, short(t.Label))
			}
		}
		if len(labels) > 0 {
			detail = fmt.Sprintf("%d of %d targets failed", len(labels), len(s.Targets))
		} else if s.Success {
			detail = fmt.Sprintf("%d targets built", len(s.Targets))
		} else {
			detail = "build incomplete or failed"
			if s.ExitName != "" {
				detail += " (" + short(s.ExitName) + ")"
			}
		}
	}
	if len(labels) > 0 {
		n := len(labels)
		if n > ListLimit {
			labels = labels[:ListLimit]
		}
		detail += ": " + strings.Join(labels, " ")
		if n > len(labels) {
			detail += fmt.Sprintf(" (+%d omitted)", n-len(labels))
		}
	}
	if !s.Complete {
		detail += "; incomplete BEP"
	}
	return prefix + detail
}

// JSON returns one bounded object. Counts describe the full snapshot; omitted
// counts include nested lists. Keep Snapshot separately for artifact lookup.
func (s Summary) JSON() ([]byte, error) {
	type view struct {
		Summary
		TargetCount       int            `json:"target_count"`
		TestCount         int            `json:"test_count"`
		FailedTestCount   int            `json:"failed_test_count"`
		FailedTargetCount int            `json:"failed_target_count"`
		Omitted           map[string]int `json:"omitted,omitempty"`
	}
	v := view{Summary: s, TargetCount: len(s.Targets), TestCount: len(s.Tests), Omitted: map[string]int{}}
	for _, t := range s.Tests {
		if !passing(t.Status) {
			v.FailedTestCount++
		}
	}
	for _, t := range s.Targets {
		if !t.Success {
			v.FailedTargetCount++
		}
	}
	capN := func(key string, n int) int {
		if n > ListLimit {
			v.Omitted[key] += n - ListLimit
			return ListLimit
		}
		return n
	}
	v.Targets = append([]Target(nil), s.Targets[:capN("targets", len(s.Targets))]...)
	for i := range v.Targets {
		t := &v.Targets[i]
		t.Outputs = t.Outputs[:capN("outputs", len(t.Outputs))]
	}
	v.Tests = append([]Test(nil), s.Tests[:capN("tests", len(s.Tests))]...)
	for i := range v.Tests {
		t := &v.Tests[i]
		t.Attempts = append([]Attempt(nil), t.Attempts[:capN("attempts", len(t.Attempts))]...)
		for j := range t.Attempts {
			a := &t.Attempts[j]
			a.Logs = a.Logs[:capN("logs", len(a.Logs))]
		}
	}
	v.FailedActions = append([]Action(nil), s.FailedActions[:capN("failed_actions", len(s.FailedActions))]...)
	for i := range v.FailedActions {
		a := &v.FailedActions[i]
		a.Message = short(a.Message)
		a.Logs = a.Logs[:capN("logs", len(a.Logs))]
	}
	return json.Marshal(v)
}
