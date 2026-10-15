package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// jobLogs feeds a job's stdout/stderr through pipes into the server-owned log files, so
// the server can stop writing at MaxLog bytes per stream (then it appends a marker and
// discards the rest, still draining the pipe so the job never blocks on it). A failed write
// (e.g. the disk is full) is reported by writeErr, and the rest is discarded likewise.
type jobLogs struct {
	Stdout, Stderr *os.File // pipe write ends, for the child
	readers        []*os.File
	files          []*os.File
	wg             sync.WaitGroup
	errs           [2]error // the first write error of each log
}

// openLogs opens dir/stdout and dir/stderr and starts the copiers. Call started once the
// child has (or has failed to) start, and wait after the job's processes are gone.
func openLogs(dir string, maxLog int64) (*jobLogs, error) {
	l := &jobLogs{}
	for i, name := range []string{"stdout", "stderr"} {
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
			l.errs[i] = copyCapped(f, r, maxLog, name)
		}()
	}
	return l, nil
}

// copyCapped copies src to dst up to max bytes (0: no limit), then appends a marker and
// discards the rest. After a failed write it discards the rest too, and returns that error.
func copyCapped(dst io.Writer, src io.Reader, max int64, name string) error {
	w := &errWriter{w: dst}
	defer io.Copy(io.Discard, src)
	if max <= 0 {
		io.Copy(w, src)
		return w.err
	}
	n, _ := io.Copy(w, io.LimitReader(src, max))
	if n < max || w.err != nil {
		return w.err // EOF (or a write error) before the cap
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k == 0 {
		return nil // exactly at the cap
	}
	fmt.Fprintf(w, "\n[tun: %s truncated: over the server's limit of %s per stream (tun serve --max-log); the rest is discarded]\n", name, fmtSize(max))
	return w.err
}

// errWriter remembers the first error of w.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// writeErr describes the logs' write errors (after wait), or is "".
func (l *jobLogs) writeErr() string {
	var msgs []string
	for i, name := range []string{"stdout", "stderr"} {
		if l.errs[i] != nil {
			msgs = append(msgs, fmt.Sprintf("writing the %s log failed, output after that is lost: %v", name, l.errs[i]))
		}
	}
	return strings.Join(msgs, "; ")
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
