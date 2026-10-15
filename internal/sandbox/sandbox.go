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

	// Session sandboxes (tun bazel, see agent.go). FixedRoot (bwrap only) replaces the
	// read-only bind of / with an empty root holding read-only binds of the host's top-level
	// entries, so Mounts can put directories at new fixed paths such as /work. Root may then
	// be empty (nothing bound at its host path).
	FixedRoot bool
	Mounts    []Mount
	// ExePath (bwrap only): where this executable is bound read-only inside the sandbox; also
	// the network helper's path. Default: a path in the tmpfs hiding DataDir.
	ExePath string
	// Pass is given to the command as file descriptor PassFD (through the network helper).
	Pass *os.File
}

// Mount binds Src (a host path) at Dst inside a bwrap sandbox.
type Mount struct {
	Src, Dst string
	RO       bool
}

// PassFD is the descriptor Spec.Pass has in the command.
const PassFD = 7

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
	files := make([]*os.File, PassFD-2) // fds 3..PassFD of the child; nil: closed
	files[PassFD-3] = s.Pass
	if s.Mode == Bwrap {
		var ours []*os.File // closed once the child has its copies
		addPipe := func(fd int, data []string) error {
			r, w, err := os.Pipe()
			if err != nil {
				return err
			}
			pipes[w] = strings.Join(data, "\x00") + "\x00"
			files[fd-3], ours = r, append(ours, r)
			return nil
		}
		defer func() {
			for _, f := range ours {
				f.Close()
			}
		}()
		args, srcs := bwrapArgs(s)
		if err := addPipe(3, args); err != nil {
			return nil, nil, err
		}
		for i, src := range srcs {
			f, err := openMountSource(src.path, src.ro)
			if err != nil {
				return nil, nil, fmt.Errorf("sandbox mount: %v", err)
			}
			for len(files) <= firstMountFD-3+i {
				files = append(files, nil)
			}
			files[firstMountFD-3+i], ours = f, append(ours, f)
		}
		argv := s.Argv
		if s.PrivateNet || s.ExePath != "" {
			exe, err := selfExe()
			if err != nil {
				return nil, nil, fmt.Errorf("binding tun into the sandbox: %v", err)
			}
			files[exeFD-3] = exe
		}
		if s.PrivateNet {
			var ctl *os.File
			var err error
			n, ctl, err = newNet()
			if err == nil {
				files[ctlFD-3], ours = ctl, append(ours, ctl)
				err = addPipe(envFD, s.Env)
			}
			if err != nil {
				closeAll()
				return nil, nil, fmt.Errorf("private network: %v", err)
			}
			argv = append([]string{helperPath(s), helperArg}, argv...)
		}
		cmd = exec.Command("bwrap", append([]string{"--args", "3", "--"}, argv...)...)
		cmd.Env = []string{}
	} else {
		cmd = exec.Command(s.Argv[0], s.Argv[1:]...)
		cmd.Dir = s.Dir
		cmd.Env = s.Env
	}
	for len(files) > 0 && files[len(files)-1] == nil {
		files = files[:len(files)-1]
	}
	cmd.ExtraFiles = files
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
func helperPath(s Spec) string {
	if s.ExePath != "" {
		return s.ExePath
	}
	return filepath.Join(s.DataDir, "tun-netns-helper")
}

// firstMountFD is the descriptor of the first bind mount source bwrap gets (see bwrapArgs).
const firstMountFD = PassFD + 1

// mountSource is a bind mount source that bwrap gets as an open descriptor.
type mountSource struct {
	path string
	ro   bool
}

// bwrapArgs: read-only root, fresh /dev /proc and pid namespace, the job's tmp at /tmp,
// the server user's home and the data dir hidden behind tmpfs, then the job's tree and shared
// caches bound back read-write at their real paths (so env paths are valid inside), and the
// job's env. Values must not contain NUL (the server rejects such jobs).
//
// The sources of the job's own binds (Tmp, Root, Caches, Mounts) are returned for Start to
// open (openMountSource: no symlink anywhere in the path, owned by this user) and pass as
// descriptors firstMountFD, firstMountFD+1, ... (--bind-fd), so a symlink a job planted in
// a directory it can write never redirects a mount, and with FixedRoot the root is
// remounted read-only once everything is in place.
func bwrapArgs(s Spec) ([]string, []mountSource) {
	var srcs []mountSource
	bind := func(src, dst string, ro bool) []string {
		srcs = append(srcs, mountSource{src, ro})
		op := "--bind-fd"
		if ro {
			op = "--ro-bind-fd"
		}
		return []string{op, strconv.Itoa(firstMountFD + len(srcs) - 1), dst}
	}
	a := []string{"--ro-bind", "/", "/"}
	if s.FixedRoot {
		a = rootBinds(s.Mounts)
	}
	a = append(a, "--dev", "/dev", "--proc", "/proc",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent")
	a = append(a, bind(s.Tmp, "/tmp", false)...)
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
	a = append(a, "--tmpfs", s.DataDir)
	if s.Root != "" {
		a = append(a, bind(s.Root, s.Root, false)...)
	}
	if s.PrivateNet {
		a = append(a, "--unshare-net")
	}
	if s.PrivateNet || s.ExePath != "" {
		a = append(a, "--ro-bind-fd", strconv.Itoa(exeFD), helperPath(s))
	}
	for _, c := range s.Caches {
		a = append(a, bind(c, c, false)...)
	}
	for _, m := range s.Mounts {
		a = append(a, bind(m.Src, m.Dst, m.RO)...)
	}
	if s.FixedRoot {
		// The root is a tmpfs holding only mount points: nothing may be written there (it
		// would live in memory until the sandbox ends). Not recursive: the binds keep theirs.
		a = append(a, "--remount-ro", "/")
	}
	a = append(a, "--clearenv")
	if s.PrivateNet {
		return append(a, "--chdir", s.Dir), srcs // the helper sets the env
	}
	for _, kv := range s.Env {
		k, v, _ := strings.Cut(kv, "=")
		a = append(a, "--setenv", k, v)
	}
	return append(a, "--chdir", s.Dir), srcs
}

const oPath = 0o10000000 // O_PATH; missing from package syscall

// openMountSource opens p (absolute) as an O_PATH descriptor one component at a time from
// /, refusing a symlink anywhere along the way, and checks that the result is a directory
// (or, for a read-only mount, a regular file) owned by this user. The descriptor then names
// that exact inode: replacing a path component later doesn't change what bwrap mounts.
func openMountSource(p string, ro bool) (*os.File, error) {
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%s: not an absolute path", p)
	}
	fd, err := syscall.Open("/", oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	for _, c := range strings.Split(filepath.Clean(p), "/")[1:] {
		next, err := syscall.Openat(fd, c, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		fd = next
		if err := syscall.Fstat(fd, &st); err != nil {
			syscall.Close(fd)
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		if st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			syscall.Close(fd)
			return nil, fmt.Errorf("%s: %q is a symlink; mount sources must not contain any", p, c)
		}
	}
	f := os.NewFile(uintptr(fd), p)
	switch typ := st.Mode & syscall.S_IFMT; {
	case typ != syscall.S_IFDIR && !(ro && typ == syscall.S_IFREG):
		err = fmt.Errorf("%s: not a directory", p)
	case int(st.Uid) != os.Getuid():
		err = fmt.Errorf("%s: owned by uid %d, not by this user (%d)", p, st.Uid, os.Getuid())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// rootBinds binds the host's top-level entries read-only into the sandbox's empty root
// (symlinks such as /bin -> usr/bin as symlinks), except /dev, /proc, /tmp (mounted fresh)
// and the top-level names the mounts use (created fresh, so they never show host content).
func rootBinds(mounts []Mount) []string {
	skip := map[string]bool{"dev": true, "proc": true, "tmp": true}
	for _, m := range mounts {
		skip[strings.SplitN(strings.TrimPrefix(m.Dst, "/"), "/", 2)[0]] = true
	}
	var a []string
	ents, _ := os.ReadDir("/")
	for _, e := range ents {
		p := "/" + e.Name()
		switch {
		case skip[e.Name()]:
		case e.Type()&os.ModeSymlink != 0:
			if t, err := os.Readlink(p); err == nil {
				a = append(a, "--symlink", t, p)
			}
		default:
			a = append(a, "--ro-bind", p, p)
		}
	}
	return a
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
