package sandbox

// Session sandboxes (tun bazel). A session is a long-lived sandbox whose first process is a
// small agent: this binary re-executed as `<exe> __tun-session-agent`. The Bazel server a
// command starts daemonizes inside the sandbox (its pid and network namespace), so it stays
// alive between commands for as long as the sandbox does. The server asks the agent to run
// each command over a SOCK_SEQPACKET socketpair (Spec.Pass, fd PassFD in the agent):
//
//	server -> agent  {"op":"run"} + the command's stdout and stderr and the read end of a
//	                 pipe as SCM_RIGHTS; the pipe then carries the request
//	                 {"argv":[...],"dir":"/work","sweep":true,"keep":"/path/server.pid.txt"}
//	                 (a pipe, not the message: argv can be larger than a socket buffer)
//	agent -> server  {"op":"started","pid":N}, later {"op":"exited","code":C|"signal":S}
//	                 (or {"op":"exited","error":...} if it could not start)
//	server -> agent  {"op":"signal","sig":N}: to the running command's process group
//
// One command at a time. When the command ends, the agent kills what is left in its process
// group and, with sweep (only in a pid namespace of its own), every other process in the
// sandbox except itself, its ancestors, and the Bazel server with its descendants, so nothing
// a command started outlives it to tamper with the next one. The Bazel server is the process
// whose pid is in the file keep when a command first ends with one there; the agent records
// it (pid and start time) and never reads the file again, since commands can write it. If
// that process is gone, nothing is spared and the exit message says "kept_gone" (the server
// then starts a new sandbox for the next command). When the server closes its end, the agent kills the running command and
// exits, which ends the sandbox (bwrap kills everything in its pid namespace).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxRequest bounds a run request (argv) read from its pipe.
const maxRequest = 16 << 20

const agentArg = "__tun-session-agent"

func init() {
	if len(os.Args) > 1 && os.Args[1] == agentArg {
		os.Exit(agentMain())
	}
}

// AgentArgv is the command that starts a session's agent: exe is where this executable is
// inside the sandbox (Spec.ExePath), or "" for a plain subprocess (sandbox none).
func AgentArgv(exe string) []string {
	if exe == "" {
		exe = "/proc/self/exe"
	}
	return []string{exe, agentArg}
}

type agentMsg struct {
	Op    string   `json:"op"`
	Argv  []string `json:"argv,omitempty"`
	Dir   string   `json:"dir,omitempty"`
	Sig   int      `json:"sig,omitempty"`
	Pid   int      `json:"pid,omitempty"`
	Code  *int     `json:"code,omitempty"`
	Sgnl  *int     `json:"signal,omitempty"`
	Error string   `json:"error,omitempty"`
	Sweep bool     `json:"sweep,omitempty"`
	Keep  string   `json:"keep,omitempty"`
	Gone  bool     `json:"kept_gone,omitempty"`
}

// procID names a process across pid reuse: its pid and its start time (/proc/<pid>/stat
// field 22, in clock ticks since boot).
type procID struct{ pid, start int }

func agentMain() int {
	f := os.NewFile(PassFD, "ctl")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tun: session agent: %v\n", err)
		return 125
	}
	ctl := c.(*net.UnixConn)
	closeOnExecAll()
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	send := func(m agentMsg) {
		b, _ := json.Marshal(m)
		ctl.WriteMsgUnix(b, nil, nil)
	}
	var mu sync.Mutex
	var cur *exec.Cmd
	var kept *procID // the Bazel server, once known (only the command goroutine uses it)
	buf, oob := make([]byte, 64<<10), make([]byte, syscall.CmsgSpace(3*4))
	for {
		k, oobn, _, _, err := ctl.ReadMsgUnix(buf, oob)
		if err != nil || k == 0 {
			mu.Lock()
			if cur != nil {
				syscall.Kill(-cur.Process.Pid, syscall.SIGKILL)
			}
			mu.Unlock()
			return 0
		}
		var files []*os.File
		if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
			for _, m := range msgs {
				fds, _ := syscall.ParseUnixRights(&m)
				for _, fd := range fds {
					syscall.CloseOnExec(fd)
					files = append(files, os.NewFile(uintptr(fd), "stdio"))
				}
			}
		}
		var m agentMsg
		if err := json.Unmarshal(buf[:k], &m); err != nil {
			m.Op = "bad"
		}
		if m.Op == "run" && len(files) == 3 {
			b, err := io.ReadAll(io.LimitReader(files[2], maxRequest+1))
			if err != nil || len(b) > maxRequest || json.Unmarshal(b, &m) != nil {
				m.Argv = nil
			}
			files[2].Close()
			files = files[:2]
		}
		mu.Lock()
		busy := cur != nil
		mu.Unlock()
		switch {
		case m.Op == "signal":
			mu.Lock()
			if cur != nil && m.Sig > 0 {
				syscall.Kill(-cur.Process.Pid, syscall.Signal(m.Sig))
			}
			mu.Unlock()
		case m.Op != "run" || len(m.Argv) == 0 || len(files) != 2 || busy:
			send(agentMsg{Op: "exited", Error: fmt.Sprintf("session agent: bad request (op %q, %d argv, %d fds, busy %v)", m.Op, len(m.Argv), len(files), busy)})
		default:
			cmd := exec.Command(m.Argv[0], m.Argv[1:]...)
			cmd.Dir, cmd.Stdout, cmd.Stderr = m.Dir, files[0], files[1]
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				send(agentMsg{Op: "exited", Error: err.Error()})
				break
			}
			mu.Lock()
			cur = cmd
			mu.Unlock()
			send(agentMsg{Op: "started", Pid: cmd.Process.Pid})
			go func() {
				cmd.Wait()
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // stragglers in its group
				r := agentMsg{Op: "exited"}
				if m.Sweep {
					if kept == nil {
						kept = readKeep(m.Keep)
					}
					r.Gone = !sweep(kept)
				}
				mu.Lock()
				cur = nil
				mu.Unlock()
				if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					n := int(ws.Signal())
					r.Sgnl = &n
				} else {
					n := cmd.ProcessState.ExitCode()
					r.Code = &n
				}
				send(r)
			}()
		}
		for _, f := range files {
			f.Close()
		}
	}
}

// readKeep reads the pid in the file keep and returns that process if it is alive.
func readKeep(keep string) *procID {
	b, err := os.ReadFile(keep)
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return nil
	}
	if st, ok := procStat(pid); ok {
		return &procID{pid, st.start}
	}
	return nil
}

type procInfo struct{ ppid, start int }

// procStat reads a live (not zombie) process's parent and start time from /proc.
func procStat(pid int) (procInfo, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	i := strings.LastIndexByte(string(b), ')')
	if err != nil || i < 0 {
		return procInfo{}, false
	}
	f := strings.Fields(string(b[i+1:])) // field 3 (state) onwards
	if len(f) < 20 || f[0] == "Z" {
		return procInfo{}, false // gone, or a zombie its parent reaps
	}
	ppid, _ := strconv.Atoi(f[1])
	start, _ := strconv.Atoi(f[19])
	return procInfo{ppid, start}, true
}

// sweep kills every process in the sandbox's pid namespace except this agent, its ancestors
// (bwrap's init, the network helper) and kept (if it is still that process) with its
// descendants. It repeats until a scan finds nothing to kill (a process may fork meanwhile).
// It reports whether kept was spared (false if kept is nil or gone).
func sweep(kept *procID) bool {
	alive := false
	for round := 0; round < 50; round++ {
		procs := map[int]procInfo{}
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if st, ok := procStat(pid); ok {
				procs[pid] = st
			}
		}
		spare := map[int]bool{}
		for p := os.Getpid(); p > 0 && !spare[p]; p = procs[p].ppid {
			spare[p] = true
		}
		alive = false
		if kept != nil {
			if st, ok := procs[kept.pid]; ok && st.start == kept.start && !spare[kept.pid] {
				alive = true
				todo := []int{kept.pid}
				for len(todo) > 0 {
					p := todo[0]
					todo = todo[1:]
					spare[p] = true
					for c, st := range procs {
						if st.ppid == p && !spare[c] {
							todo = append(todo, c)
						}
					}
				}
			}
		}
		killed := 0
		for pid := range procs {
			if !spare[pid] {
				syscall.Kill(pid, syscall.SIGKILL)
				killed++
			}
		}
		if killed == 0 {
			break
		}
		time.Sleep(time.Duration(round+1) * time.Millisecond)
	}
	return alive
}

// Session is the server's handle on a running session sandbox.
type Session struct {
	cmd  *exec.Cmd
	net  *Net
	ctl  *net.UnixConn
	msgs chan agentMsg // from the agent; closed when the channel to it fails
	done chan struct{} // closed once the sandbox process has exited
	mu   sync.Mutex    // one command at a time
}

// StartSession starts a session sandbox for s (Argv: AgentArgv). The sandbox's own output
// (bwrap's, the agent's, daemons that write to inherited stdio) goes to log.
func StartSession(s Spec, log *os.File) (*Session, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	theirs := os.NewFile(uintptr(fds[1]), "session-ctl")
	mine := os.NewFile(uintptr(fds[0]), "session-ctl")
	c, err := net.FileConn(mine)
	mine.Close()
	if err != nil {
		theirs.Close()
		return nil, err
	}
	s.Pass = theirs
	cmd, n, err := Start(s, log, log)
	theirs.Close()
	if err != nil {
		c.Close()
		return nil, err
	}
	ss := &Session{cmd: cmd, net: n, ctl: c.(*net.UnixConn), msgs: make(chan agentMsg, 4), done: make(chan struct{})}
	go func() {
		defer close(ss.msgs)
		buf := make([]byte, 64<<10)
		for {
			k, _, _, _, err := ss.ctl.ReadMsgUnix(buf, nil)
			if err != nil || k == 0 {
				return
			}
			var m agentMsg
			if json.Unmarshal(buf[:k], &m) == nil {
				ss.msgs <- m
			}
		}
	}()
	go func() {
		cmd.Wait()
		if n != nil {
			n.Close()
		}
		ss.ctl.Close()
		close(ss.done)
	}()
	return ss, nil
}

// Done is closed once the sandbox has exited.
func (ss *Session) Done() <-chan struct{} { return ss.done }

// Result is how a session command ended: exactly one of Code, Signal set, or Err.
// KeptGone: with RunOpts.Sweep, the Bazel server the agent spares is gone (or was never
// found), so the sweep killed every other process; start a new sandbox for the next command.
type Result struct {
	Code, Signal *int
	Err          error
	KeptGone     bool
}

// ErrSessionGone means the session sandbox exited (or stopped answering) under a command.
var ErrSessionGone = errors.New("session sandbox exited")

// RunOpts are the optional parts of a command: with Sweep (only for a sandbox with its own
// pid namespace) every process the command leaves behind is killed when it ends, except the
// Bazel server and its descendants: the process whose pid was in the file Keep (a path inside
// the sandbox) when a command of this sandbox first ended with a live one there (see sweep).
type RunOpts struct {
	Sweep bool
	Keep  string
}

// Run starts argv in dir (a path inside the sandbox) with the given stdout/stderr and
// returns a channel that delivers its result once. The caller may close its copies of the
// files once Run returns. Only one command runs at a time; Run fails if one is running.
func (ss *Session) Run(argv []string, dir string, stdout, stderr *os.File, o RunOpts) (<-chan Result, error) {
	if !ss.mu.TryLock() {
		return nil, errors.New("session is running another command")
	}
	req, _ := json.Marshal(agentMsg{Op: "run", Argv: argv, Dir: dir, Sweep: o.Sweep, Keep: o.Keep})
	if len(req) > maxRequest {
		ss.mu.Unlock()
		return nil, fmt.Errorf("the command line is %d bytes, over the limit of %d", len(req), maxRequest)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		ss.mu.Unlock()
		return nil, err
	}
	b, _ := json.Marshal(agentMsg{Op: "run"})
	_, _, err = ss.ctl.WriteMsgUnix(b, syscall.UnixRights(int(stdout.Fd()), int(stderr.Fd()), int(pr.Fd())), nil)
	pr.Close()
	if err != nil {
		pw.Close()
		ss.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrSessionGone, err)
	}
	go func() { // the agent reads it all before it answers; EPIPE if it is gone
		pw.Write(req)
		pw.Close()
	}()
	next := func(timeout <-chan time.Time) (agentMsg, error) {
		select {
		case m, ok := <-ss.msgs:
			if !ok {
				return m, ErrSessionGone
			}
			return m, nil
		case <-timeout:
			return agentMsg{}, fmt.Errorf("%w: no answer from the session agent", ErrSessionGone)
		}
	}
	m, err := next(time.After(30 * time.Second))
	if err != nil {
		// The agent may still answer later; replies must never attach to another command.
		ss.Kill()
		ss.mu.Unlock()
		return nil, err
	}
	if m.Op != "started" {
		ss.mu.Unlock()
		return nil, fmt.Errorf("starting %s: %s", argv[0], m.Error)
	}
	res := make(chan Result, 1)
	go func() {
		defer ss.mu.Unlock()
		m, err := next(nil)
		switch {
		case err != nil:
			res <- Result{Err: err}
		case m.Error != "":
			res <- Result{Err: errors.New(m.Error)}
		default:
			res <- Result{Code: m.Code, Signal: m.Sgnl, KeptGone: m.Gone}
		}
	}()
	return res, nil
}

// Signal sends sig to the running command's process group (no-op if none runs).
func (ss *Session) Signal(sig syscall.Signal) {
	b, _ := json.Marshal(agentMsg{Op: "signal", Sig: int(sig)})
	ss.ctl.WriteMsgUnix(b, nil, nil)
}

// Close ends the sandbox: it closes the channel to the agent (which kills its command and
// exits), waits up to grace for the sandbox to exit, then kills its process group.
func (ss *Session) Close(grace time.Duration) {
	ss.ctl.CloseWrite()
	select {
	case <-ss.done:
		return
	case <-time.After(grace):
	}
	syscall.Kill(-ss.cmd.Process.Pid, syscall.SIGKILL)
	<-ss.done
}

// Kill ends the sandbox at once.
func (ss *Session) Kill() {
	syscall.Kill(-ss.cmd.Process.Pid, syscall.SIGKILL)
	<-ss.done
}
