package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// jobLogs feeds a job's stdout/stderr through pipes into the server-owned log files, so
// the server can stop writing at MaxLog bytes per stream (then it appends a marker and
// discards the rest, still draining the pipe so the job never blocks on it).
type jobLogs struct {
	Stdout, Stderr *os.File // pipe write ends, for the child
	readers        []*os.File
	files          []*os.File
	wg             sync.WaitGroup
}

// openLogs opens dir/stdout and dir/stderr and starts the copiers. Call started once the
// child has (or has failed to) start, and wait after the job's processes are gone.
func (s *Server) openLogs(dir string) (*jobLogs, error) {
	l := &jobLogs{}
	for _, name := range []string{"stdout", "stderr"} {
		f, err := openOwn(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			l.close()
			return nil, err
		}
		l.files = append(l.files, f)
		r, w, err := os.Pipe()
		if err != nil {
			l.close()
			return nil, err
		}
		l.readers = append(l.readers, r)
		if name == "stdout" {
			l.Stdout = w
		} else {
			l.Stderr = w
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			copyCapped(f, r, s.cfg.MaxLog, name)
		}()
	}
	return l, nil
}

func copyCapped(dst io.Writer, src io.Reader, max int64, name string) {
	if max <= 0 {
		io.Copy(dst, src)
		return
	}
	n, _ := io.Copy(dst, io.LimitReader(src, max))
	if n < max {
		return // EOF (or a write error) before the cap
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k == 0 {
		return // exactly at the cap
	}
	fmt.Fprintf(dst, "\n[tun: %s truncated: over the server's limit of %s per stream (tun serve --max-log); the rest is discarded]\n", name, fmtSize(max))
	io.Copy(io.Discard, src)
}

// started closes the parent's copies of the pipe write ends.
func (l *jobLogs) started() {
	for _, w := range []*os.File{l.Stdout, l.Stderr} {
		if w != nil {
			w.Close()
		}
	}
}

// wait lets the copiers drain what the job wrote, giving up after a few seconds if some
// escaped process still holds a pipe open, then closes everything.
func (l *jobLogs) wait() {
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	l.close()
	<-done
}

func (l *jobLogs) close() {
	l.started()
	for _, f := range append(l.readers, l.files...) {
		f.Close()
	}
}
