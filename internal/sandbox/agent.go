package sandbox

// Session sandboxes (tun bazel). A session is a long-lived sandbox whose first process is a
// small agent: this binary re-executed as `<exe> __tun-session-agent`. The Bazel server a
// command starts daemonizes inside the sandbox (its pid and network namespace), so it stays
// alive between commands for as long as the sandbox does. The server asks the agent to run
// each command over a SOCK_SEQPACKET socketpair (Spec.Pass, fd PassFD in the agent):
//
//	server -> agent  {"op":"run","argv":[...],"dir":"/work"} + the command's stdout and
//	                 stderr as SCM_RIGHTS
//	agent -> server  {"op":"started","pid":N}, later {"op":"exited","code":C|"signal":S}
//	                 (or {"op":"exited","error":...} if it could not start)
//	server -> agent  {"op":"signal","sig":N}: to the running command's process group
//
// One command at a time. When the server closes its end, the agent kills the running
// command and exits, which ends the sandbox (bwrap kills everything in its pid namespace).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

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
}

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
	buf, oob := make([]byte, 1<<20), make([]byte, syscall.CmsgSpace(2*4))
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
				mu.Lock()
				cur = nil
				mu.Unlock()
				r := agentMsg{Op: "exited"}
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
type Result struct {
	Code, Signal *int
	Err          error
}

// ErrSessionGone means the session sandbox exited (or stopped answering) under a command.
var ErrSessionGone = errors.New("session sandbox exited")

// Run starts argv in dir (a path inside the sandbox) with the given stdout/stderr and
// returns a channel that delivers its result once. The caller may close its copies of the
// files once Run returns. Only one command runs at a time; Run fails if one is running.
func (ss *Session) Run(argv []string, dir string, stdout, stderr *os.File) (<-chan Result, error) {
	if !ss.mu.TryLock() {
		return nil, errors.New("session is running another command")
	}
	b, _ := json.Marshal(agentMsg{Op: "run", Argv: argv, Dir: dir})
	if _, _, err := ss.ctl.WriteMsgUnix(b, syscall.UnixRights(int(stdout.Fd()), int(stderr.Fd())), nil); err != nil {
		ss.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrSessionGone, err)
	}
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
			res <- Result{Code: m.Code, Signal: m.Sgnl}
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
