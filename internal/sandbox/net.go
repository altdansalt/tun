package sandbox

// Private networking for bwrap jobs. With Spec.PrivateNet, bwrap gives the job its own network
// namespace (only a loopback interface), so jobs can't reach each other's ports or the host's.
// The server can't enter that namespace itself (unprivileged, multithreaded), so bwrap runs a
// small helper first: this binary re-executed as `<exe> __tun-netns-helper <argv...>`. Inside
// the namespace it
//   - reads the job's env from a pipe (envFD), so that e.g. LD_PRELOAD doesn't act on it;
//   - listens on 127.0.0.1:<random> and points HTTP(S)_PROXY there (unless the job env sets
//     them), then runs argv as its child and exits with the child's status;
//   - answers requests on a socketpair shared with the server (fd 4) by sending back file
//     descriptors: the egress listener ("egress"), and TCP connections it dials to
//     127.0.0.1:<port> in the namespace ("dial <port>", for the service proxy and probe).
// The server accepts on the egress listener and serves an HTTP proxy (CONNECT and absolute-URI
// requests) that dials out from the host's network. So jobs keep HTTP(S) access to the internet
// (module downloads, git over https) but have no raw outbound TCP/UDP and no DNS of their own.

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	helperArg = "__tun-netns-helper"
	ctlFD     = 4 // the helper's end of the control socketpair
	exeFD     = 5 // the tun executable, bound into the sandbox for the helper
	envFD     = 6 // the job's env, NUL-terminated K=V entries
)

// Every binary importing this package (tun, and test binaries that run servers) can be the helper.
func init() {
	if len(os.Args) > 1 && os.Args[1] == helperArg {
		os.Exit(helperMain(os.Args[2:]))
	}
}

// selfExe is an O_PATH handle on the running executable, so the helper is this exact binary
// even if the file on disk is replaced while the server runs.
var selfExe = sync.OnceValues(func() (*os.File, error) {
	return os.OpenFile("/proc/self/exe", oPath|syscall.O_CLOEXEC, 0)
})

// EgressDial makes the egress proxy's outbound TCP connections to host:port. Tests replace it.
var EgressDial = dialOut

// NetUsable reports (as an error, with bwrap's output) whether jobs can get a private network
// namespace here: it starts a real sandboxed job with the helper and asks for its egress listener.
func NetUsable() error {
	dir, err := os.MkdirTemp("", "tun-netcheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return err
	}
	var out strings.Builder
	cmd, n, err := Start(Spec{Mode: Bwrap, PrivateNet: true, Argv: []string{"sleep", "10"}, Env: []string{"PATH=/usr/bin:/bin"}, Dir: dir, Root: dir, Tmp: dir, DataDir: dir}, &out, &out)
	if err != nil {
		return err
	}
	f, err := n.call("egress")
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait()
	n.Close()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	f.Close()
	return nil
}

// Net is the server's handle on a job's private network namespace.
type Net struct {
	mu  sync.Mutex // one request/response at a time on ctl
	ctl *net.UnixConn
	hs  *http.Server // the egress proxy
}

func newNet() (*Net, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	mine := os.NewFile(uintptr(fds[0]), "tun-netns-ctl")
	c, err := net.FileConn(mine)
	mine.Close()
	if err != nil {
		syscall.Close(fds[1])
		return nil, nil, err
	}
	n := &Net{ctl: c.(*net.UnixConn), hs: &http.Server{Handler: http.HandlerFunc(egress), ReadHeaderTimeout: time.Minute}}
	return n, os.NewFile(uintptr(fds[1]), "tun-netns-ctl"), nil
}

// serveEgress gets the egress listener from the helper and serves the proxy on it until Close.
func (n *Net) serveEgress() {
	f, err := n.call("egress")
	if err != nil {
		return // the job didn't start or the helper is gone; Dial reports such errors
	}
	ln, err := net.FileListener(f)
	f.Close()
	if err == nil {
		n.hs.Serve(ln) // closes ln; returns at once if Close came first
	}
}

// Dial connects to 127.0.0.1:port inside the job's network namespace.
func (n *Net) Dial(port int) (net.Conn, error) {
	f, err := n.call("dial " + strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return net.FileConn(f)
}

// Close stops the egress proxy and drops the control connection. Call it once the job ended.
func (n *Net) Close() {
	n.hs.Close()
	n.ctl.Close()
}

func (n *Net) call(req string) (*os.File, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ctl.SetDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := n.ctl.WriteMsgUnix([]byte(req), nil, nil); err != nil {
		return nil, fmt.Errorf("network helper: %v", err)
	}
	buf, oob := make([]byte, 1024), make([]byte, syscall.CmsgSpace(4))
	k, oobn, _, _, err := n.ctl.ReadMsgUnix(buf, oob) // Go sets MSG_CMSG_CLOEXEC
	if err != nil {
		return nil, fmt.Errorf("network helper: %v", err)
	}
	if k == 0 {
		return nil, errors.New("network helper exited")
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	var fds []int
	if err == nil && len(msgs) == 1 {
		fds, err = syscall.ParseUnixRights(&msgs[0])
	}
	if string(buf[:k]) != "ok" || err != nil || len(fds) != 1 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return nil, errors.New(string(buf[:k]))
	}
	return os.NewFile(uintptr(fds[0]), req), nil
}

// egress is the job's HTTP proxy: CONNECT tunnels and absolute-URI requests, dialed from the
// host network.
func egress(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		up, err := EgressDial(r.Context(), r.Host)
		if err != nil {
			http.Error(w, "tun egress proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			up.Close()
			return
		}
		rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
		if rw.Flush() == nil {
			relay(c, rw.Reader, up)
		}
		c.Close()
		up.Close()
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "tun egress proxy: only CONNECT and absolute-URI requests (set HTTP_PROXY/HTTPS_PROXY)", http.StatusBadRequest)
		return
	}
	forward.ServeHTTP(w, r)
}

var forward = &httputil.ReverseProxy{
	Rewrite:       func(*httputil.ProxyRequest) {}, // Out.URL is already the absolute target
	FlushInterval: -1,
	Transport: &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		return EgressDial(ctx, addr)
	}, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second},
	ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "tun egress proxy: "+err.Error(), http.StatusBadGateway)
	},
}

// relay copies both ways until either side is done.
func relay(c net.Conn, cr io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, cr); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
}

var dialer = &net.Dialer{Timeout: 30 * time.Second}

// dialOut connects to addr, through a CONNECT tunnel if the server's own environment names an
// HTTPS_PROXY for it (e.g. a tun server running inside a tun job).
func dialOut(ctx context.Context, addr string) (net.Conn, error) {
	pu, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil || pu == nil {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	host := pu.Host
	if pu.Port() == "" {
		host = net.JoinHostPort(pu.Hostname(), "80")
	}
	c, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if pu.User != nil {
		pw, _ := pu.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pw)) + "\r\n"
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	var resp *http.Response
	if _, err = io.WriteString(c, req+"\r\n"); err == nil {
		resp, err = http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	}
	if err == nil && resp.StatusCode != 200 {
		err = fmt.Errorf("upstream proxy %s: CONNECT %s: %s", pu.Host, addr, resp.Status)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return &bufConn{c, br}, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// helperMain runs inside the sandbox's network namespace; see the top of this file.
func helperMain(argv []string) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "tun: network helper: %v\n", err)
		return 125
	}
	if len(argv) == 0 {
		return fail(errors.New("no command"))
	}
	envPipe := os.NewFile(envFD, "env")
	b, err := io.ReadAll(envPipe)
	envPipe.Close()
	if err != nil {
		return fail(err)
	}
	os.Clearenv()
	for _, kv := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			os.Setenv(k, v)
		}
	}
	f := os.NewFile(ctlFD, "ctl")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return fail(err)
	}
	ctl := c.(*net.UnixConn)
	var pass *os.File // Spec.Pass, handed on to the command
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, PassFD, syscall.F_GETFD, 0); errno == 0 {
		pass = os.NewFile(PassFD, "pass")
	}
	closeOnExecAll() // e.g. the bound executable's fd; the command gets only stdio (and pass)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	env := os.Environ()
	setDefault := func(v string, keys ...string) {
		for _, k := range keys {
			if _, ok := os.LookupEnv(k); !ok {
				env = append(env, k+"="+v)
			}
		}
	}
	setDefault("http://"+ln.Addr().String(), "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy")
	setDefault("localhost,127.0.0.1,::1", "NO_PROXY", "no_proxy")

	// Signals for the job's process group reach the command directly; just don't die of them.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	if pass != nil {
		cmd.ExtraFiles = make([]*os.File, PassFD-2)
		cmd.ExtraFiles[PassFD-3] = pass
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tun: %v\n", err)
		return 127
	}
	if pass != nil {
		pass.Close() // the command's end of the server's channel is the command's alone
	}
	go serveCtl(ctl, ln.(*net.TCPListener))
	cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() {
		return ws.ExitStatus()
	}
	// Die of the same signal, bypassing the Go runtime's handlers (a zeroed sigaction is SIG_DFL).
	var dfl [4]uint64
	syscall.RawSyscall6(syscall.SYS_RT_SIGACTION, uintptr(ws.Signal()), uintptr(unsafe.Pointer(&dfl)), 0, 8, 0, 0)
	syscall.Kill(os.Getpid(), ws.Signal())
	time.Sleep(time.Second)
	return 128 + int(ws.Signal())
}

// serveCtl answers the server's requests until the control connection closes.
func serveCtl(ctl *net.UnixConn, ln *net.TCPListener) {
	buf := make([]byte, 64)
	for {
		k, _, _, _, err := ctl.ReadMsgUnix(buf, nil)
		if err != nil || k == 0 {
			return
		}
		var f *os.File
		req := string(buf[:k])
		if req == "egress" {
			f, err = ln.File()
		} else if p, ok := strings.CutPrefix(req, "dial "); ok {
			var c net.Conn
			if c, err = net.DialTimeout("tcp", "127.0.0.1:"+p, 2*time.Second); err == nil {
				f, err = c.(*net.TCPConn).File()
				c.Close()
			}
		} else {
			err = fmt.Errorf("unknown request %q", req)
		}
		if err != nil {
			ctl.WriteMsgUnix([]byte(err.Error()), nil, nil)
			continue
		}
		ctl.WriteMsgUnix([]byte("ok"), syscall.UnixRights(int(f.Fd())), nil)
		f.Close()
	}
}

func closeOnExecAll() {
	ents, _ := os.ReadDir("/proc/self/fd")
	for _, e := range ents {
		if fd, err := strconv.Atoi(e.Name()); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
}
