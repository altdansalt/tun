package reapi

import (
	"context"
	"fmt"
	"github.com/altdansalt/tun/internal/sandbox"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionOperations(t *testing.T) {
	h, addr, _ := testServer(t)
	var calls atomic.Int32
	h.Execution = NewExecution(h.Store, 2, func(context.Context, []byte, []byte) ([]byte, error) { calls.Add(1); return intField(4, 0), nil })
	defer h.Execution.Close()
	put := func(b []byte) []byte {
		d := digest(b)
		if err := h.Store.put("cas-", d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	command := put(bytesField(1, []byte("true")))
	root := put(nil)
	action := put(msg(bytesField(1, command), bytesField(2, root)))
	var name string
	for i := 0; i < 3; i++ {
		req := msg(bytesField(6, action), intField(3, int64(i/2)))
		o, c, err := h.Execution.start(req)
		if err != nil || c != 0 {
			t.Fatal(c, err)
		}
		name = o.name
		code, err := h.Execution.stream(context.Background(), "/WaitExecution", bytesField(1, []byte(name)), func(b []byte) error {
			if get(b, 3).v != 0 {
				a := get(b, 5).b
				if string(get(a, 1).b) != executionType+"ExecuteResponse" {
					t.Fatalf("wrong Any: %x", a)
				}
			}
			return nil
		})
		if err != nil || code != 0 {
			t.Fatal(code, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("cache lookup / skip: %d calls", calls.Load())
	}
	b, c := rpc(t, addr, "/build.bazel.remote.execution.v2.Execution/WaitExecution", "secret", bytesField(1, []byte(name)))
	if c != "0" || get(b, 3).v != 1 {
		t.Fatalf("wait: %x %s", b, c)
	}
	_, c = rpc(t, addr, "/build.bazel.remote.execution.v2.Execution/WaitExecution", "secret", bytesField(1, []byte("missing")))
	if c != "5" {
		t.Fatal(c)
	}
	if err := validatePlatform(bytesField(1, msg(bytesField(1, []byte("container-image")), bytesField(2, []byte("docker://x"))))); err == nil {
		t.Fatal("accepted container")
	}
	for _, p := range []string{"../x", "/x", "a/../../x"} {
		if safeRelative(p) == nil {
			t.Fatal(p)
		}
	}
}

func TestBazelExecution(t *testing.T) {
	if _, err := exec.LookPath("bazel"); err != nil {
		t.Skip("bazel missing")
	}
	h, addr, _ := testServer(t)
	data := t.TempDir()
	mode := sandbox.None
	if sandbox.BwrapUsable() {
		mode = sandbox.Bwrap
	}
	h.Execution = NewExecution(h.Store, 2, LocalExecutor(h.Store, data, mode))
	defer h.Execution.Close()
	h.AllowActionWrites = false
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "MODULE.bazel"), nil, 0600)
	os.WriteFile(filepath.Join(work, "BUILD.bazel"), []byte(`load(":tree.bzl", "tree_rule")
tree_rule(name="tree")
genrule(name="leaf", outs=["leaf.txt"], cmd="echo remote-output > $@")
genrule(name="top", srcs=[":leaf", ":tree"], outs=["top.txt"], cmd="cat $(location :leaf) $(location :tree)/sub/file > $@")
genrule(name="bad", outs=["bad.txt"], cmd="echo deliberate-action-failure >&2; exit 1")`), 0600)
	os.WriteFile(filepath.Join(work, "tree.bzl"), []byte(`def _tree(ctx):
    out = ctx.actions.declare_directory(ctx.label.name + ".dir")
    ctx.actions.run_shell(outputs=[out], command="mkdir -p %s/sub; echo tree-output > %s/sub/file" % (out.path, out.path))
    return [DefaultInfo(files=depset([out]))]
tree_rule = rule(implementation=_tree)
`), 0600)
	run := func(target string, wantSuccess bool) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		args := []string{"--output_user_root=" + t.TempDir(), "--host_jvm_args=-Xmx512m", "--batch", "build", target, "--jobs=2", "--remote_executor=grpc://" + addr, "--remote_header=x-tun-token=secret", "--remote_timeout=15", "--noremote_upload_local_results", "--enable_bzlmod=false"}
		cmd := exec.CommandContext(ctx, "bazel", args...)
		cmd.Dir = work
		cmd.WaitDelay = 3 * time.Second
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("bazel: %v\n%s", err, out)
		}
		return string(out)
	}
	out := run("//:top", true)
	if !strings.Contains(out, "remote") {
		t.Fatal(out)
	}
	b, err := os.ReadFile(filepath.Join(work, "bazel-bin", "top.txt"))
	if err != nil || string(b) != "remote-output\ntree-output\n" {
		t.Fatalf("output %q %v", b, err)
	}
	out = run("//:top", true)
	if !strings.Contains(out, "remote cache hit") {
		t.Fatal(out)
	}
	out = run("//:bad", false)
	if !strings.Contains(out, "deliberate-action-failure") {
		t.Fatal(out)
	}
}

func TestExecutorFilesystem(t *testing.T) {
	store, err := Open(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) []byte {
		d := digest(b)
		if err := store.put("cas-", d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	input := put([]byte("input-data"))
	dir := put(bytesField(1, msg(bytesField(1, []byte("in")), bytesField(2, input))))
	root := put(bytesField(2, msg(bytesField(1, []byte("sub")), bytesField(2, dir))))
	action := msg(bytesField(2, root), bytesField(6, intField(1, 5)))
	for _, field := range []int{3, 7} {
		t.Run(fmt.Sprint(field), func(t *testing.T) {
			command := msg(bytesField(1, []byte("/bin/sh")), bytesField(1, []byte("-c")), bytesField(1, []byte(`test "$ONLY" = specified; test -z "$TUN_JOB_ID"; mkdir -p tree; cat in > tree/out; echo stderr >&2; cp in file`)), bytesField(2, msg(bytesField(1, []byte("ONLY")), bytesField(2, []byte("specified")))), bytesField(6, []byte("sub")), bytesField(field, []byte("file")))
			dirField := 4
			if field == 7 {
				dirField = 7
			}
			command = append(command, bytesField(dirField, []byte("tree"))...)
			result, err := LocalExecutor(store, t.TempDir(), "none")(context.Background(), action, command)
			if err != nil {
				t.Fatal(err)
			}
			if get(result, 4).v != 0 || len(all(result, 2)) != 1 || len(all(result, 3)) != 1 || !store.actionAvailable(result) {
				t.Fatalf("bad result %x", result)
			}
			data, err := store.read("cas-", get(get(result, 2).b, 2).b)
			if err != nil || string(data) != "input-data" {
				t.Fatalf("output %q %v", data, err)
			}
		})
	}
	command := msg(bytesField(1, []byte("/bin/sleep")), bytesField(1, []byte("10")))
	action = msg(bytesField(2, root), bytesField(6, intField(2, 10000000)))
	start := time.Now()
	_, err = LocalExecutor(store, t.TempDir(), "none")(context.Background(), action, command)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("timeout: %v %s", err, time.Since(start))
	}
}

func TestDoNotCacheAndReconnect(t *testing.T) {
	store, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) []byte {
		d := digest(b)
		if err := store.put("cas-", d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	command := put(bytesField(1, []byte("true")))
	root := put(nil)
	d := put(msg(bytesField(1, command), bytesField(2, root), intField(7, 1)))
	started, finish := make(chan struct{}), make(chan struct{})
	e := NewExecution(store, 1, func(context.Context, []byte, []byte) ([]byte, error) {
		close(started)
		<-finish
		return intField(4, 0), nil
	})
	defer e.Close()
	// Simulate a failed Execute stream, then reconnect to the same operation.
	var name string
	_, err = e.stream(context.Background(), "/Execute", bytesField(6, d), func(b []byte) error { name = string(get(b, 1).b); return io.ErrClosedPipe })
	if err == nil {
		t.Fatal("expected disconnect")
	}
	<-started
	close(finish)
	_, err = e.stream(context.Background(), "/WaitExecution", bytesField(1, []byte(name)), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.read("ac-", d); !os.IsNotExist(err) {
		t.Fatalf("do_not_cache wrote AC: %v", err)
	}
}

func TestRemoteExecutorInputCache(t *testing.T) {
	hub, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	put := func(b []byte) []byte {
		d := digest(b)
		if err := hub.PutBlob(d, b); err != nil {
			t.Fatal(err)
		}
		return d
	}
	file := put([]byte("original"))
	root := put(bytesField(1, msg(bytesField(1, []byte("in")), bytesField(2, file))))
	fetches := 0
	remote := &RemoteStore{Local: local, Fetch: func(d []byte) ([]byte, error) { fetches++; return hub.ReadBlob(d) }, Upload: hub.PutBlob}
	command := msg(bytesField(1, []byte("/bin/sh")), bytesField(1, []byte("-c")), bytesField(1, []byte("cat in > out; echo mutated > in")), bytesField(7, []byte("out")))
	execute := LocalExecutor(remote, t.TempDir(), "none")
	for i := 0; i < 2; i++ {
		result, err := execute(context.Background(), bytesField(2, root), command)
		if err != nil {
			t.Fatal(err)
		}
		out, err := hub.ReadBlob(get(get(result, 2).b, 2).b)
		if err != nil || string(out) != "original" {
			t.Fatalf("CAS modified: %q %v", out, err)
		}
	}
	if fetches != 2 {
		t.Fatalf("re-downloaded inputs: %d fetches, want directory + file", fetches)
	}
}

func TestExecutorIsolation(t *testing.T) {
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap unavailable")
	}
	if err := sandbox.NetUsable(); err != nil {
		t.Skipf("private network unavailable: %v", err)
	}
	store, err := Open(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	root := digest(nil)
	if err = store.PutBlob(root, nil); err != nil {
		t.Fatal(err)
	}
	action := bytesField(2, root)
	execute := LocalExecutor(store, t.TempDir(), sandbox.Bwrap)
	// No proxy, HOME, PATH, or tun job environment may be injected by the executor.
	command := msg(bytesField(1, []byte("/usr/bin/env")), bytesField(2, msg(bytesField(1, []byte("ONLY")), bytesField(2, []byte("specified")))))
	result, err := execute(context.Background(), action, command)
	if err != nil {
		t.Fatal(err)
	}
	out, err := store.ReadBlob(get(result, 6).b)
	if err != nil || string(out) != "ONLY=specified\n" {
		t.Fatalf("environment %q %v", out, err)
	}
	command = msg(bytesField(1, []byte("/usr/bin/readlink")), bytesField(1, []byte("/proc/self/ns/net")))
	result, err = execute(context.Background(), action, command)
	if err != nil {
		t.Fatal(err)
	}
	out, err = store.ReadBlob(get(result, 6).b)
	host, _ := os.Readlink("/proc/self/ns/net")
	if err != nil || strings.TrimSpace(string(out)) == host {
		t.Fatalf("network not isolated: %q host=%s error=%v", out, host, err)
	}
}
