package reapi

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxMessage = 4 << 20

// Handler serves uncompressed protobuf gRPC. AllowActionWrites is the policy seam
// for restricting ActionCache updates to executor credentials in M3.
//
// The CAS is shared (content is verified). The action cache has namespaces: results a client
// uploads go to that client's own namespace and only answer its lookups; the shared namespace
// ("") is read by everyone after their own and written only by tun's executors (not over
// gRPC). So no client can poison another's builds. A request's namespace comes from the
// server, never from the request's instance_name.
type Handler struct {
	Store             *Store
	Execution         *Execution
	Token             string
	AllowActionWrites bool
	// Client names the action cache namespace of an authenticated request (its client).
	// Nil or "": the shared namespace, read-only.
	Client      func(*http.Request) string
	trusted     bool          // authenticated by the listener (ForClient): no token
	idleTimeout time.Duration // zero uses two minutes
}

// ForClient returns a handler for a listener that only client can reach (a session's own
// socket): requests need no token and use client's action cache namespace.
func (h *Handler) ForClient(client string) *Handler {
	c := *h
	c.trusted, c.Client = true, func(*http.Request) string { return client }
	return &c
}

func Protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}
func frameRead(r io.Reader) ([]byte, error) {
	var h [5]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return nil, e
	}
	n := binary.BigEndian.Uint32(h[1:])
	if h[0] != 0 || n > maxMessage {
		return nil, fmt.Errorf("compressed or oversized message")
	}
	b := make([]byte, n)
	_, e := io.ReadFull(r, b)
	if e == nil {
		_, e = decode(b)
	}
	return b, e
}
func frameWrite(w http.ResponseWriter, b []byte) error {
	var h [5]byte
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
	if _, e := w.Write(h[:]); e != nil {
		return e
	}
	_, e := w.Write(b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return e
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "grpc-status, grpc-message")
	code := 0
	message := ""
	defer func() {
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		w.Header().Set("grpc-status", strconv.Itoa(code))
		w.Header().Set("grpc-message", url.PathEscape(message))
	}()
	fail := func(c int, e error) {
		code = c
		if os.IsTimeout(e) {
			code = 4
		}
		message = e.Error()
	}
	token := r.Header.Get("X-Tun-Token")
	if token == "" {
		token, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if !h.trusted && subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) != 1 {
		fail(16, fmt.Errorf("missing or wrong x-tun-token"))
		return
	}
	ns := ""
	if h.Client != nil {
		ns = h.Client(r)
	}
	if r.Method != "POST" || r.ProtoMajor != 2 {
		fail(3, fmt.Errorf("gRPC requires HTTP/2 POST"))
		return
	}
	idle := h.idleTimeout
	if idle == 0 {
		idle = 2 * time.Minute
	}
	var deadline time.Time
	if t := r.Header.Get("grpc-timeout"); t != "" {
		if len(t) < 2 || len(t) > 9 {
			fail(3, fmt.Errorf("invalid grpc-timeout"))
			return
		}
		units := map[byte]time.Duration{'H': time.Hour, 'M': time.Minute, 'S': time.Second, 'm': time.Millisecond, 'u': time.Microsecond, 'n': time.Nanosecond}
		n, e := strconv.ParseInt(t[:len(t)-1], 10, 32)
		u := units[t[len(t)-1]]
		if e != nil || u == 0 || n < 0 {
			fail(3, fmt.Errorf("invalid grpc-timeout"))
			return
		}
		if n <= int64((1<<63-1)/u) {
			deadline = time.Now().Add(time.Duration(n) * u)
		}
	}
	rc := http.NewResponseController(w)
	// The idle limit is our own timer, reset for each message; when it fires it closes the
	// request body, which ends a pending read. Moving the HTTP/2 read deadline for every
	// message instead sometimes ended an active stream under load ("i/o timeout" well before
	// the limit; HTTP/2 applies deadline updates asynchronously, which may be why).
	var (
		timerMu sync.Mutex
		timer   *time.Timer
		gen     int
		expired bool
	)
	stopTimer := func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		gen++
		if timer != nil {
			timer.Stop()
		}
	}
	defer stopTimer()
	refresh := func() error {
		now := time.Now()
		if !deadline.IsZero() && !now.Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		next := now.Add(idle)
		if !deadline.IsZero() && deadline.Before(next) {
			next = deadline
		}
		timerMu.Lock()
		defer timerMu.Unlock()
		gen++
		g := gen
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(next.Sub(now), func() {
			timerMu.Lock()
			defer timerMu.Unlock()
			if g == gen {
				expired = true
				r.Body.Close() // ends a pending read
			}
		})
		return nil
	}
	read := func() ([]byte, error) {
		if e := refresh(); e != nil {
			return nil, e
		}
		b, err := frameRead(r.Body)
		timerMu.Lock()
		timedOut := expired
		timerMu.Unlock()
		// HTTP/2 may return an opaque timeout error without net.Error.
		if err != nil && timedOut {
			return nil, os.ErrDeadlineExceeded
		}
		return b, err
	}
	write := func(b []byte) error {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		// Bound stalled consumers, refreshing the idle limit for each message.
		_ = rc.SetWriteDeadline(time.Now().Add(idle))
		err := frameWrite(w, b)
		_ = rc.SetWriteDeadline(time.Time{})
		return err
	}
	b, e := read()
	if e != nil {
		fail(3, e)
		return
	}
	method := r.URL.Path
	if method != "/google.bytestream.ByteStream/Write" {
		if _, err := read(); err != io.EOF {
			if err == nil {
				err = fmt.Errorf("unary request must contain exactly one message")
			}
			fail(3, err)
			return
		}
	}
	// Unary input is complete; Read/GetTree may keep producing output for hours.
	if method != "/google.bytestream.ByteStream/Write" {
		stopTimer()
	}
	var out []byte
	switch method {
	case "/build.bazel.remote.execution.v2.Execution/Execute", "/build.bazel.remote.execution.v2.Execution/WaitExecution":
		if h.Execution == nil {
			fail(12, fmt.Errorf("execution disabled"))
			return
		}
		ctx := r.Context()
		if !deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
		c, err := h.Execution.stream(ctx, method, b, write)
		if err != nil {
			fail(c, err)
		}
		return
	case "/build.bazel.remote.execution.v2.Capabilities/GetCapabilities":
		enabled := int64(0)
		if h.AllowActionWrites && ns != "" {
			enabled = 1
		}
		out = msg(bytesField(1, msg(intField(1, 1), bytesField(2, intField(1, enabled)), intField(4, 2<<20), intField(8, h.Store.max))), bytesField(4, intField(1, 2)), bytesField(5, intField(1, 2)))
		if h.Execution != nil {
			out = append(out, bytesField(2, msg(intField(1, 1), intField(2, 1)))...)
		}
	case "/build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs":
		for _, d := range all(b, 2) {
			if _, e := key(d); e != nil {
				fail(3, e)
				return
			}
			if !h.Store.has(d) {
				out = append(out, bytesField(2, d)...)
			}
		}
	case "/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchUpdateBlobs":
		for _, x := range all(b, 2) {
			d := get(x, 1).b
			var e error
			if get(x, 3).v != 0 {
				e = fmt.Errorf("unsupported compressor")
			} else {
				e = h.Store.put("cas-", d, get(x, 2).b)
			}
			c := int64(0)
			if e != nil {
				c = 3
				if errors.Is(e, errCapacity) {
					c = 8
				}
			}
			out = append(out, bytesField(1, msg(bytesField(1, d), bytesField(2, intField(1, c))))...)
		}
	case "/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchReadBlobs":
		var total uint64
		for _, d := range all(b, 2) {
			size := get(d, 2).v
			if size > 2<<20 || total+size > 2<<20 {
				fail(3, fmt.Errorf("batch exceeds 2MiB"))
				return
			}
			total += size
		}
		for _, d := range all(b, 2) {
			_, digestErr := key(d)
			var data []byte
			var e error
			if digestErr == nil {
				data, e = h.Store.read("cas-", d)
			}
			c := int64(0)
			if e != nil {
				c = 5
			}
			if digestErr != nil {
				c = 3
			}
			out = append(out, bytesField(1, msg(bytesField(1, d), bytesField(2, data), bytesField(3, intField(1, c))))...)
		}
	case "/build.bazel.remote.execution.v2.ActionCache/GetActionResult":
		k, e := key(get(b, 2).b)
		if e != nil {
			fail(3, e)
			return
		}
		// The client's own result first, then the shared (executor-written) one.
		e = os.ErrNotExist
		for _, n := range []string{ns, ""} {
			if out, e = h.Store.readName(actionName(n, k)); e == nil && !h.Store.actionAvailable(out) {
				e = os.ErrNotExist
			}
			if e == nil || n == "" {
				break
			}
		}
		if e != nil {
			fail(5, e)
			return
		}
	case "/build.bazel.remote.execution.v2.ActionCache/UpdateActionResult":
		if !h.AllowActionWrites || ns == "" {
			fail(7, fmt.Errorf("action cache writes disabled"))
			return
		}
		k, e := key(get(b, 2).b)
		if e != nil {
			fail(3, e)
			return
		}
		out = get(b, 3).b
		if _, err := decode(out); err != nil {
			fail(3, err)
			return
		}
		if e = h.Store.putName(actionName(ns, k), out); e != nil {
			fail(3, e)
			return
		}
	case "/build.bazel.remote.execution.v2.ContentAddressableStorage/GetTree":
		queue := [][]byte{get(b, 2).b}
		seen := map[string]bool{}
		for len(queue) > 0 {
			d := queue[0]
			queue = queue[1:]
			k, e := key(d)
			if e != nil {
				fail(3, e)
				return
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			data, e := h.Store.read("cas-", d)
			if e != nil {
				fail(5, e)
				return
			}
			if e = write(bytesField(1, data)); e != nil {
				fail(13, e)
				return
			}
			for _, node := range all(data, 2) {
				queue = append(queue, get(node, 2).b)
			}
		}
		return
	case "/google.bytestream.ByteStream/Read":
		d, e := resource(string(get(b, 1).b), false)
		if e != nil {
			fail(3, e)
			return
		}

		if isEmptyDigest(d) {
			if get(b, 2).v != 0 || get(b, 3).v > 1<<63-1 {
				fail(11, fmt.Errorf("read range out of bounds"))
			}
			return
		}
		f, e := h.Store.openBlob(d)
		if e != nil {
			fail(5, e)
			return
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			fail(13, e)
			return
		}
		offset, limit := get(b, 2).v, get(b, 3).v
		if offset > uint64(info.Size()) || limit > 1<<63-1 {
			fail(11, fmt.Errorf("read range out of bounds"))
			return
		}
		n := info.Size() - int64(offset)
		if limit > 0 && int64(limit) < n {
			n = int64(limit)
		}
		reader := io.NewSectionReader(f, int64(offset), n)
		buf := make([]byte, 64<<10)
		for {
			n, e := reader.Read(buf)
			if n > 0 {
				if err := write(bytesField(10, buf[:n])); err != nil {
					fail(13, err)
					return
				}
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				fail(13, e)
				return
			}
		}
		return
	case "/google.bytestream.ByteStream/Write":
		name := string(get(b, 1).b)
		d, e := resource(name, true)
		if e != nil {
			fail(3, e)
			return
		}

		for {
			if n := string(get(b, 1).b); n != "" && n != name {
				fail(3, fmt.Errorf("resource changed"))
				return
			}
			count, err := h.Store.appendUpload(name, d, get(b, 2).v, get(b, 10).b, get(b, 3).v != 0)
			if err != nil {
				c := 3
				if errors.Is(err, errCapacity) {
					c = 8
				}
				if errors.Is(err, errOffset) {
					c = 11
				}
				fail(c, err)
				return
			}
			if get(b, 3).v != 0 {
				out = intField(1, count)
				break
			}
			b, e = read()
			if e != nil {
				fail(3, e)
				return
			}
		}

	case "/google.bytestream.ByteStream/QueryWriteStatus":
		name := string(get(b, 1).b)
		d, e := resource(name, true)
		if e != nil {
			fail(3, e)
			return
		}
		if h.Store.has(d) {
			out = msg(intField(1, int64(get(d, 2).v)), intField(2, 1))
		} else {

			p := h.Store.uploadPath(name)
			lock := h.Store.fileLock(filepath.Base(p))
			lock.Lock()
			info, err := os.Stat(p)
			lock.Unlock()
			if err != nil {
				fail(5, os.ErrNotExist)
				return
			}
			out = intField(1, info.Size())
		}
	default:
		fail(12, fmt.Errorf("unknown method"))
		return
	}
	if e = write(out); e != nil {
		fail(13, e)
		return
	}
}
func resource(name string, upload bool) ([]byte, error) {
	p := strings.Split(name, "/")
	n := len(p)
	if n < 3 || p[n-3] != "blobs" {
		return nil, fmt.Errorf("invalid blob resource")
	}
	if upload && (n < 5 || p[n-5] != "uploads" || p[n-4] == "") {
		return nil, fmt.Errorf("invalid upload resource")
	}
	size, e := strconv.ParseInt(p[n-1], 10, 64)
	if e != nil || size < 0 {
		return nil, fmt.Errorf("invalid blob size")
	}
	d := msg(bytesField(1, []byte(p[n-2])), intField(2, size))
	_, e = key(d)
	return d, e
}
