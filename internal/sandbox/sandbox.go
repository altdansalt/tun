// Package sandbox builds the command that runs a job: under bubblewrap (bwrap) when
// available, else as a plain subprocess. Either way the job gets its own process group so
// timeouts and cancel can kill the whole tree.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Modes.
const (
	Bwrap = "bwrap"
	None  = "none"
)

// Spec describes one job's execution environment. All paths are absolute.
type Spec struct {
	Mode    string
	Argv    []string
	Env     []string
	Dir     string   // working directory (inside Root)
	Root    string   // read-write: the job's own tree (work, home, tmp); nothing server-owned
	Tmp     string   // the job's tmp dir; also mounted at /tmp under bwrap
	DataDir string   // the server data dir: hidden under bwrap except Root and Caches
	Caches  []string // read-write shared cache dirs
	// PrivateNet (bwrap only) gives the job its own network namespace; see net.go.
	PrivateNet bool
}

// BwrapUsable reports whether bwrap is installed and can create the namespaces we need.
func BwrapUsable() bool {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--tmpfs", "/tmp", "--unshare-pid", "--die-with-parent", "true").Run() == nil
}

// Start starts the command for s in its own process group, with the given stdout/stderr.
//
// Under bwrap, the job's env is applied inside the sandbox (--clearenv/--setenv) and bwrap
// itself runs with an empty env, so client-supplied vars like LD_PRELOAD can't act on the
// unsandboxed bwrap process. The options travel over a pipe (--args) rather than argv, so
// env values don't show up in the host's process list.
//
// With s.PrivateNet the returned Net reaches into the job's network namespace; the caller
// must Close it once the command has ended. It is nil otherwise. The network helper also gets
// the env over a pipe (envFD) rather than --setenv, so it doesn't act on the helper either.
func Start(s Spec, stdout, stderr io.Writer) (*exec.Cmd, *Net, error) {
	var cmd *exec.Cmd
	var n *Net
	pipes := map[*os.File]string{} // write ends, and what to write once the command started
	closeAll := func() {
		for w := range pipes {
			w.Close()
		}
		if n != nil {
			n.Close()
		}
	}
	if s.Mode == Bwrap {
		var files, ours []*os.File // ours: closed once the child has its copies
		addPipe := func(data []string) error {
			r, w, err := os.Pipe()
			if err != nil {
				return err
			}
			pipes[w] = strings.Join(data, "\x00") + "\x00"
			files, ours = append(files, r), append(ours, r)
			return nil
		}
		defer func() {
			for _, f := range ours {
				f.Close()
			}
		}()
		if err := addPipe(bwrapArgs(s)); err != nil {
			return nil, nil, err
		}
		argv := s.Argv
		if s.PrivateNet {
			exe, err := selfExe()
			var ctl *os.File
			if err == nil {
				n, ctl, err = newNet()
			}
			if err == nil {
				files, ours = append(files, ctl, exe), append(ours, ctl) // ctlFD, exeFD
				err = addPipe(s.Env)                                     // envFD
			}
			if err != nil {
				closeAll()
				return nil, nil, fmt.Errorf("private network: %v", err)
			}
			argv = append([]string{helperPath(s), helperArg}, argv...)
		}
		cmd = exec.Command("bwrap", append([]string{"--args", "3", "--"}, argv...)...)
		cmd.Env = []string{}
		cmd.ExtraFiles = files
	} else {
		cmd = exec.Command(s.Argv[0], s.Argv[1:]...)
		cmd.Dir = s.Dir
		cmd.Env = s.Env
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, nil, err
	}
	for w, data := range pipes {
		go func() { // the pipe may be smaller than the env; the reader reads it all first
			io.WriteString(w, data)
			w.Close()
		}()
	}
	if n != nil {
		go n.serveEgress()
	}
	return cmd, n, nil
}

// helperPath is where the network helper (this executable) appears inside the sandbox: in
// the tmpfs that hides the data dir.
func helperPath(s Spec) string { return filepath.Join(s.DataDir, "tun-netns-helper") }

// bwrapArgs: read-only root, fresh /dev /proc and pid namespace, the job's tmp at /tmp,
// the server user's home and the data dir hidden behind tmpfs, then the job's tree and shared
// caches bound back read-write at their real paths (so env paths are valid inside), and the
// job's env. Values must not contain NUL (the server rejects such jobs).
func bwrapArgs(s Spec) []string {
	a := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent",
		"--bind", s.Tmp, "/tmp"}
	if home, err := os.UserHomeDir(); err == nil && home != "/" && filepath.IsAbs(home) {
		// Hide the server user's home (it may hold the client token, ssh keys, ...), but keep
		// toolchains on PATH that live under it (e.g. ~/go/bin -> ~/go) readable.
		a = append(a, "--tmpfs", home)
		for _, p := range filepath.SplitList(os.Getenv("PATH")) {
			if !within(p, home) || p == home {
				continue
			}
			if filepath.Base(p) == "bin" && filepath.Dir(p) != home {
				p = filepath.Dir(p)
			}
			if _, err := os.Stat(p); err == nil {
				a = append(a, "--ro-bind", p, p)
			}
		}
	}
	a = append(a, "--tmpfs", s.DataDir, "--bind", s.Root, s.Root)
	if s.PrivateNet {
		a = append(a, "--unshare-net", "--ro-bind-fd", strconv.Itoa(exeFD), helperPath(s))
	}
	for _, c := range s.Caches {
		a = append(a, "--bind", c, c)
	}
	a = append(a, "--clearenv")
	if s.PrivateNet {
		return append(a, "--chdir", s.Dir) // the helper sets the env
	}
	for _, kv := range s.Env {
		k, v, _ := strings.Cut(kv, "=")
		a = append(a, "--setenv", k, v)
	}
	return append(a, "--chdir", s.Dir)
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
