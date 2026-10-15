// Package sandbox builds the command that runs a job: under bubblewrap (bwrap) when
// available, else as a plain subprocess. Either way the job gets its own process group so
// timeouts and cancel can kill the whole tree.
package sandbox

import (
	"context"
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
	Dir     string   // working directory (inside JobDir)
	JobDir  string   // read-write: the job's own dir (work, home, tmp)
	Tmp     string   // the job's tmp dir; also mounted at /tmp under bwrap
	DataDir string   // the server data dir: hidden under bwrap except JobDir and Caches
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

// Command returns an unstarted command for s, in its own process group.
func Command(s Spec) *exec.Cmd {
	var cmd *exec.Cmd
	if s.Mode == Bwrap {
		cmd = exec.Command("bwrap", append(bwrapArgs(s), s.Argv...)...)
	} else {
		cmd = exec.Command(s.Argv[0], s.Argv[1:]...)
		cmd.Dir = s.Dir
	}
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	return cmd
}

// bwrapArgs: read-only root, fresh /dev /proc and pid namespace, the job's tmp at /tmp,
// the server user's home and the data dir hidden behind tmpfs, then the job dir and shared
// caches bound back read-write at their real paths (so env paths are valid inside).
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
	a = append(a, "--tmpfs", s.DataDir, "--bind", s.JobDir, s.JobDir)
	for _, c := range s.Caches {
		a = append(a, "--bind", c, c)
	}
	return append(a, "--chdir", s.Dir, "--")
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
