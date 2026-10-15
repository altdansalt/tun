package reapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExecutionIdleAndRandomNames(t *testing.T) {
	h, addr, _ := testServer(t)
	h.idleTimeout = 30 * time.Millisecond
	put := func(b []byte) []byte {
		d := digest(b)
		if err := h.Store.PutBlob(d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	command := put(bytesField(1, []byte("true")))
	action := put(msg(bytesField(1, command), bytesField(2, put(nil))))
	run := func(context.Context, []byte, []byte) ([]byte, error) {
		time.Sleep(150 * time.Millisecond)
		return intField(4, 0), nil
	}
	h.Execution = NewExecution(h.Store, 1, run)
	o, _, err := h.Execution.start(msg(bytesField(6, action), intField(3, 1)))
	if err != nil {
		t.Fatal(err)
	}
	name := o.name
	if len(name) != len("operations/")+32 {
		t.Fatal(name)
	}
	_, code := rpc(t, addr, "/build.bazel.remote.execution.v2.Execution/WaitExecution", "secret", bytesField(1, []byte(name)))
	if code != "0" {
		t.Fatal(code)
	}
	h.Execution.Close()
	h.Execution = NewExecution(h.Store, 1, run)
	defer h.Execution.Close()
	other, _, err := h.Execution.start(bytesField(6, action))
	if err != nil {
		t.Fatal(err)
	}
	if other.name == name {
		t.Fatal("reused name")
	}
	c, _ := h.Execution.stream(context.Background(), "/WaitExecution", bytesField(1, []byte(name)), func([]byte) error { return nil })
	if c != 5 {
		t.Fatal(c)
	}
	h.Execution.mu.Lock()
	other.completed = time.Now().Add(-2 * time.Hour)
	h.Execution.mu.Unlock()
	_, _, err = h.Execution.start(bytesField(6, action))
	if err != nil {
		t.Fatal(err)
	}
	h.Execution.mu.Lock()
	_, exists := h.Execution.ops[other.name]
	h.Execution.mu.Unlock()
	if exists {
		t.Fatal("old completed operation retained")
	}
}

func TestExecutorReviewLimits(t *testing.T) {
	s, err := Open(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) []byte {
		d := digest(b)
		if err := s.PutBlob(d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	empty := put(nil)
	rootDir := t.TempDir()
	run := LocalExecutor(s, rootDir, "none")
	cmd := func(script string) []byte {
		return msg(bytesField(1, []byte("/bin/sh")), bytesField(1, []byte("-c")), bytesField(1, []byte(script)), bytesField(7, []byte("out")))
	}
	for _, script := range []string{"yes", "truncate -s 100000000000 out"} {
		_, err := run(context.Background(), bytesField(2, empty), cmd(script))
		var ex *ExecutionError
		if !errors.As(err, &ex) || ex.Code != 8 {
			t.Fatalf("%s: %v", script, err)
		}
	}
	entries, _ := os.ReadDir(rootDir)
	if len(entries) != 0 {
		t.Fatalf("leaked roots: %v", entries)
	}
	// A small reused Directory DAG expands exponentially without a node budget.
	d := empty
	for i := 0; i < 18; i++ {
		d = put(msg(bytesField(2, msg(bytesField(1, []byte("a")), bytesField(2, d))), bytesField(2, msg(bytesField(1, []byte("b")), bytesField(2, d)))))
	}
	r, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Start near the limit to keep the regression fast.
	err = materializeContext(context.Background(), s, r, ".", d, 0, &inputBudget{nodes: 99990})
	var ex *ExecutionError
	if !errors.As(err, &ex) || ex.Code != 8 {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = materializeContext(ctx, s, r, ".", empty, 0, &inputBudget{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if timeout, err := actionTimeout(bytesField(6, nil)); err != nil || timeout != 30*time.Minute {
		t.Fatal(timeout, err)
	}
}

func TestMissingInputDetailsAndUntrustedCache(t *testing.T) {
	s, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) []byte {
		d := digest(b)
		if err := s.PutBlob(d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	command := put(bytesField(1, []byte("/bin/true")))
	missing := digest([]byte("absent"))
	root := put(bytesField(1, msg(bytesField(1, []byte("input")), bytesField(2, missing))))
	action := put(msg(bytesField(1, command), bytesField(2, root)))
	e := NewExecution(s, 1, LocalExecutor(s, t.TempDir(), "none"))
	defer e.Close()
	var response []byte
	_, err = e.stream(context.Background(), "/Execute", bytesField(6, action), func(b []byte) error {
		if get(b, 3).v != 0 {
			response = get(get(b, 5).b, 2).b
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	status := get(response, 3).b
	if get(status, 1).v != 9 || len(all(status, 3)) != 1 {
		t.Fatalf("status %x", status)
	}
	action = put(msg(bytesField(1, command), bytesField(2, put(nil))))
	_, err = e.stream(context.Background(), "/Execute", bytesField(6, action), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.read("ac-", action); !os.IsNotExist(err) {
		t.Fatal("unsafe result cached", err)
	}
	// Same-size local CAS corruption must fail verification.
	d := put([]byte("good"))
	k, _ := key(d)
	if err = os.WriteFile(filepath.Join(s.dir, "cas-"+k), []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadBlob(d); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestSweepReadOnlyExecRoots(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "action-old")
	if err := os.MkdirAll(filepath.Join(root, "readonly"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readonly", "x"), nil, 0444); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(root, "readonly"), 0555)
	if err := SweepExecRoots(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
