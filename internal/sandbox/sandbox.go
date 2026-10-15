// Package sandbox builds the command that runs a job: under bubblewrap (bwrap) when
// available, else as a plain subprocess. Either way the job gets its own process group so
// timeouts and cancel can kill the whole tree.
package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
func Start(s Spec, stdout, stderr io.Writer) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	var opts *os.File
	if s.Mode == Bwrap {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		opts = w
		defer r.Close() // the child has its own copy once started
		cmd = exec.Command("bwrap", append([]string{"--args", "3", "--"}, s.Argv...)...)
		cmd.Env = []string{}
		cmd.ExtraFiles = []*os.File{r}
	} else {
		cmd = exec.Command(s.Argv[0], s.Argv[1:]...)
		cmd.Dir = s.Dir
		cmd.Env = s.Env
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		if opts != nil {
			opts.Close()
		}
		return nil, err
	}
	if opts != nil {
		data := strings.Join(bwrapArgs(s), "\x00") + "\x00"
		go func() { // the pipe may be smaller than the env; bwrap reads it all before anything else
			io.WriteString(opts, data)
			opts.Close()
		}()
	}
	return cmd, nil
}

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
	for _, c := range s.Caches {
		a = append(a, "--bind", c, c)
	}
	a = append(a, "--clearenv")
	for _, kv := range s.Env {
		k, v, _ := strings.Cut(kv, "=")
		a = append(a, "--setenv", k, v)
	}
	return append(a, "--chdir", s.Dir)
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
