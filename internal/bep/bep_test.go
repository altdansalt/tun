package bep

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) Summary {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := New("query")
	// Exercise growing-file reads across every possible kind of token boundary.
	for i := 0; i < len(b); i += 17 {
		end := i + 17
		if end > len(b) {
			end = len(b)
		}
		if _, err := p.Write(b[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if name == "query" {
		p.SetResult(0, time.Second)
	}
	return p.Snapshot()
}
func TestFixtures(t *testing.T) {
	for _, tc := range []struct {
		name             string
		code             int
		status           string
		attempts, shards int
	}{
		{"success", 0, "", 0, 0}, {"build_failure", 1, "", 0, 0}, {"test_failure", 3, "FAILED", 1, 1}, {"flaky", 0, "FLAKY", 2, 1}, {"sharded", 0, "PASSED", 3, 3}, {"timeout", 3, "TIMEOUT", 1, 1}, {"analysis_failure", 1, "", 0, 0}, {"query", 0, "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, tc.name)
			if s.ExitCode == nil || *s.ExitCode != tc.code || s.Success != (tc.code == 0) || !s.Complete || s.Duration <= 0 {
				t.Fatalf("bad result: %+v", s)
			}
			if tc.status != "" {
				if len(s.Tests) != 1 {
					t.Fatal(s.Tests)
				}
				test := s.Tests[0]
				if test.Status != tc.status || len(test.Attempts) != tc.attempts || test.Shards != tc.shards || test.Duration <= 0 {
					t.Fatalf("bad test: %+v", test)
				}
				for _, a := range test.Attempts {
					if len(a.Logs) < 1 || !strings.HasPrefix(a.Logs[0].Path, "/") {
						t.Fatal(a)
					}
				}
			}
			if tc.name == "success" && (len(s.Targets) != 1 || s.Targets[0].Outputs[0].Path != "bazel-bin/hello.txt") {
				t.Fatal(s.Targets)
			}
			if tc.name == "build_failure" && (len(s.FailedActions) != 1 || s.FailedActions[0].Message == "" || s.Targets[0].Success) {
				t.Fatal(s)
			}
			if tc.name == "query" && len(s.Targets) != 0 {
				t.Fatal(s)
			}
			line := s.Line()
			if !strings.HasPrefix(line, fmt.Sprintf("tun: bazel %s exit=%d after ", s.Command, tc.code)) || strings.Contains(line, "\n") {
				t.Fatal(line)
			}
			if tc.status == "FAILED" && !strings.Contains(line, "1 of 1 tests failed: //:fail") {
				t.Fatal(line)
			}
			b, err := s.JSON()
			if err != nil || !json.Valid(b) {
				t.Fatalf("%s %v", b, err)
			}
		})
	}
}
func TestPartialAndUnknown(t *testing.T) {
	p := New("build")
	p.Write([]byte("{\"unknown\":true}\n{\"started\":"))
	if p.Snapshot().Complete {
		t.Fatal("premature completion")
	}
	if p.Finish() == nil {
		t.Fatal("expected truncated JSON error")
	}
	p.SetResult(137, 2*time.Second)
	if *p.Snapshot().ExitCode != 137 {
		t.Fatal(p.Snapshot())
	}
	p = New("test")
	p.Write([]byte(`{"id":{"testSummary":{"label":"//:x"}},"testSummary":{"overallStatus":"FUTURE_STATUS"}}`))
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Snapshot().Line(), "tests failed") {
		t.Fatal(p.Snapshot())
	}
}
func TestBoundedRendering(t *testing.T) {
	s := Summary{Command: "test"}
	for i := 0; i < 100; i++ {
		s.Tests = append(s.Tests, Test{Label: fmt.Sprintf("//:test%d", i), Status: "FAILED"})
	}
	if !strings.Contains(s.Line(), "(+80 omitted)") {
		t.Fatal(s.Line())
	}
	b, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Tests     []Test
		TestCount int `json:"test_count"`
		Omitted   map[string]int
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Tests) != 20 || v.TestCount != 100 || v.Omitted["tests"] != 80 {
		t.Fatal(string(b))
	}
	if len(s.Tests) != 100 {
		t.Fatal("mutated snapshot")
	}
}
func TestNamedSets(t *testing.T) {
	p := New("build")
	_, err := p.Write([]byte(`{"id":{"targetCompleted":{"label":"//:x"}},"completed":{"success":true,"outputGroup":[{"name":"default","fileSets":[{"id":"a"}]}]}}
{"id":{"namedSet":{"id":"a"}},"namedSetOfFiles":{"fileSets":[{"id":"b"}]}}
{"id":{"namedSet":{"id":"b"}},"namedSetOfFiles":{"fileSets":[{"id":"a"}],"files":[{"name":"x","pathPrefix":["bazel-out","k8-fastbuild","bin"],"uri":"file:///tmp/x"}]}}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot()
	if len(s.Targets[0].Outputs) != 1 || s.Targets[0].Outputs[0].Path != "bazel-bin/x" {
		t.Fatal(s)
	}
}
