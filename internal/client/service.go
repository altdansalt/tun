package client

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// serviceUsage is appended to tun run -h.
const serviceUsage = `
services (--service): the job runs until canceled (no --timeout unless given), outside the
--max-jobs queue (the server's --max-services bounds them; creating one more fails). Each
--port N gets a free host port, passed to the job as $TUN_PORT_N (and $PORT if only one);
the service must listen there on 127.0.0.1. tun run waits until every port accepts
connections, prints to stdout the job id, then one line "N URL" per port, and exits 0. URL
is <server>/v1/jobs/<id>/ports/N/ and proxies HTTP (streaming, websockets) to the service;
it needs the same "Authorization: Bearer <token>" header as the API, which is not forwarded.
If the service exits first, tun run prints its output and exits with its code (125 if 0);
if it isn't ready within --ready-timeout it is canceled. Stop it with 'tun cancel <id>'.
`

type portFlag []int

func (f *portFlag) String() string { return fmt.Sprint(*f) }
func (f *portFlag) Set(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("want a port number 1-65535")
	}
	*f = append(*f, n)
	return nil
}

type serviceOpts struct {
	service      *bool
	ports        portFlag
	readyTimeout *time.Duration
}

func serviceFlags(fs *flag.FlagSet) *serviceOpts {
	o := &serviceOpts{}
	o.service = fs.Bool("service", false, "run a long-lived service (see below); tun run exits once it is ready")
	fs.Var(&o.ports, "port", "with --service: expose port `N` (repeatable) via the server's proxy")
	o.readyTimeout = fs.Duration("ready-timeout", 2*time.Minute, "with --service: cancel the service if its ports don't accept connections by then")
	return o
}

// apply validates the service flags and sets them on req. Services get no timeout unless
// --timeout was given explicitly.
func (o *serviceOpts) apply(fs *flag.FlagSet, req *api.CreateJob) error {
	if len(o.ports) > 0 && !*o.service {
		return fmt.Errorf("run: --port needs --service")
	}
	if !*o.service {
		return nil
	}
	if *o.readyTimeout <= 0 {
		return fmt.Errorf("run: --ready-timeout must be positive")
	}
	req.Service, req.Ports = true, o.ports
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "timeout" })
	if !explicit {
		req.TimeoutSec = 0
	}
	return nil
}

// withURLs fills in each port's full proxy URL.
func withURLs(c *Client, j *api.Job) {
	for i := range j.Ports {
		j.Ports[i].URL = strings.TrimRight(c.URL, "/") + j.Ports[i].Path
	}
}

// waitReady waits for a service job to accept connections on all its ports, then prints
// its id and URLs. If it ends first, it prints the job's output and returns its exit code.
func waitReady(ctx context.Context, c *Client, j api.Job, readyTimeout time.Duration, jsonOut bool, e Env) int {
	deadline := time.Now().Add(readyTimeout)
	logs := func() {
		c.Logs(ctx, j.ID, "stdout", 0, false, e.Stdout)
		c.Logs(ctx, j.ID, "stderr", 0, false, e.Stderr)
	}
	for {
		cur, err := c.Get(ctx, j.ID)
		if err != nil {
			return errorf(e, "getting status of service %s: %v (it may still be running: tun status %s)", j.ID, err, j.ID)
		}
		ready := cur.State == api.Running
		var waiting []string
		for _, p := range cur.Ports {
			if !p.Ready {
				ready = false
				waiting = append(waiting, strconv.Itoa(p.Port))
			}
		}
		switch {
		case api.Terminal(cur.State):
			logs()
			code, msg := exitCode(cur)
			if code == 0 {
				code = 125
			}
			if msg == "" {
				msg = fmt.Sprintf("exited with code %d", *cur.ExitCode)
			}
			fmt.Fprintf(e.Stderr, "tun: service %s %s before it was ready\n", cur.ID, msg)
			return code
		case ready:
			withURLs(c, &cur)
			if jsonOut {
				printJSON(e, cur)
			} else {
				fmt.Fprintln(e.Stdout, cur.ID)
				for _, p := range cur.Ports {
					fmt.Fprintf(e.Stdout, "%d %s\n", p.Port, p.URL)
				}
				fmt.Fprintf(e.Stderr, "tun: service %s ready; output: tun logs -f %s; stop: tun cancel %s\n", cur.ID, cur.ID, cur.ID)
			}
			return 0
		case time.Now().After(deadline):
			c.Cancel(ctx, j.ID)
			logs()
			return errorf(e, "service %s not accepting connections on port(s) %s after %v (--ready-timeout); canceled it. It must listen on 127.0.0.1:$PORT (or $TUN_PORT_<N>)", j.ID, strings.Join(waiting, ","), readyTimeout)
		}
		select {
		case sig := <-e.Signals:
			cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer ccancel()
			if _, err := c.Cancel(cctx, j.ID); err != nil {
				return errorf(e, "received %v but could not cancel service %s: %v", sig, j.ID, err)
			}
			fmt.Fprintf(e.Stderr, "tun: received %v; canceled service %s\n", sig, j.ID)
			if s, ok := sig.(syscall.Signal); ok {
				return 128 + int(s)
			}
			return 125
		case <-time.After(100 * time.Millisecond):
		}
	}
}
