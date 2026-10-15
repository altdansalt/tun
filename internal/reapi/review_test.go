package reapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRejectedUploadStatus(t *testing.T) {
	h, addr, _ := testServer(t)
	data := []byte("hello")
	d := digest(data)
	name := "uploads/bad/blobs/" + string(get(d, 1).b) + "/5"
	if _, err := h.Store.appendUpload(name, d, 2, nil, false); err != errOffset {
		t.Fatal(err)
	}
	query := func(want string) {
		t.Helper()
		_, code := rpc(t, addr, "/google.bytestream.ByteStream/QueryWriteStatus", "secret", bytesField(1, []byte(name)))
		if code != want {
			t.Fatalf("status %s want %s", code, want)
		}
	}
	query("5")
	if _, err := h.Store.appendUpload(name, d, 0, data[:2], false); err != nil {
		t.Fatal(err)
	}
	query("0")
	if _, err := h.Store.appendUpload(name, d, 2, data, false); err != errCapacity {
		t.Fatal(err)
	}
	query("5")
}

func TestBatchResourceExhausted(t *testing.T) {
	h, addr, _ := testServer(t)
	h.Store.max = 1
	data := []byte("large")
	b, c := rpc(t, addr, "/build.bazel.remote.execution.v2.ContentAddressableStorage/BatchUpdateBlobs", "secret", bytesField(2, msg(bytesField(1, digest(data)), bytesField(2, data))))
	if c != "0" || get(get(get(b, 1).b, 2).b, 1).v != 8 {
		t.Fatalf("%x %s", b, c)
	}
}

func TestEvictionFailureContinues(t *testing.T) {
	s, err := Open(t.TempDir(), 6)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := []byte("aaa"), []byte("bbb"), []byte("ccc")
	if err = s.put("cas-", digest(a), a); err != nil {
		t.Fatal(err)
	}
	// Replace the oldest file with a nonempty directory to force Remove to fail,
	// even when tests run as root. It must not poison subsequent writes.
	p := filepath.Join(s.dir, "cas-"+string(get(digest(a), 1).b))
	if err = os.Remove(p); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(p, 0700)
	os.WriteFile(filepath.Join(p, "child"), nil, 0600)
	for _, data := range [][]byte{b, c, []byte("ddd")} {
		if err = s.put("cas-", digest(data), data); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	total := s.total
	s.mu.Unlock()
	if total > s.max {
		t.Fatalf("eviction stopped: %d", total)
	}
}

// HTTP/2 reports an expired read deadline as fmt.Errorf("%w", os.ErrDeadlineExceeded); when it
// fires before the handler's own record of the deadline (SetReadDeadline is applied
// asynchronously), the stream used to end with INVALID_ARGUMENT instead of DEADLINE_EXCEEDED.
func TestGRPCCodeWrappedTimeout(t *testing.T) {
	if c := grpcCode(3, fmt.Errorf("%w", os.ErrDeadlineExceeded)); c != 4 {
		t.Errorf("wrapped deadline: %d", c)
	}
	if c := grpcCode(3, io.ErrUnexpectedEOF); c != 3 {
		t.Errorf("other error: %d", c)
	}
}

// Real HTTP/2 streaming requests exercise trailer status after a read timeout.
func TestStreamDeadlinesAndBearer(t *testing.T) {
	for _, tc := range []struct {
		name, timeout, token string
		idle, pause          time.Duration
		chunks               int
		want                 string
	}{
		{"bearer", "", "secret", 100 * time.Millisecond, 0, 1, "0"},
		{"wrong bearer", "", "wrong", 100 * time.Millisecond, 0, 1, "16"},
		{"client expires", "20m", "secret", 100 * time.Millisecond, 60 * time.Millisecond, 2, "4"},
		{"idle expires", "", "secret", 100 * time.Millisecond, 180 * time.Millisecond, 2, "4"},
		// Each gap is far below the idle limit (a loaded host stretches a 50ms sleep, never
		// shortens it), while the whole stream outlasts it; checked below.
		{"active beyond idle", "30S", "secret", time.Second, 50 * time.Millisecond, 25, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, addr, _ := testServer(t)
			h.idleTimeout = tc.idle
			start := time.Now()
			data := bytes.Repeat([]byte("x"), tc.chunks)
			d := digest(data)
			name := "uploads/deadline/blobs/" + string(get(d, 1).b) + fmt.Sprintf("/%d", len(data))
			reader, writer := io.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer writer.Close()
				for i := 0; i < tc.chunks; i++ {
					if i > 0 {
						time.Sleep(tc.pause)
					}
					b := msg(intField(2, int64(i)), bytesField(10, []byte("x")))
					if i == 0 {
						b = append(b, bytesField(1, []byte(name))...)
					}
					if i == tc.chunks-1 {
						b = append(b, intField(3, 1)...)
					}
					var frame [5]byte
					binary.BigEndian.PutUint32(frame[1:], uint32(len(b)))
					if _, err := writer.Write(append(frame[:], b...)); err != nil {
						return
					}
				}
			}()
			defer func() { reader.Close(); <-done }()
			req, _ := http.NewRequest("POST", "http://"+addr+"/google.bytestream.ByteStream/Write", reader)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.timeout != "" {
				req.Header.Set("grpc-timeout", tc.timeout)
			}
			protocols := new(http.Protocols)
			protocols.SetUnencryptedHTTP2(true)
			tr := &http.Transport{Protocols: protocols}
			defer tr.CloseIdleConnections()
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Trailer.Get("grpc-status"); got != tc.want {
				t.Fatalf("got %s want %s trailers=%v", got, tc.want, resp.Trailer)
			}
			if tc.want == "0" && tc.chunks > 1 && time.Since(start) <= tc.idle {
				t.Fatalf("stream took %v, not beyond the %v idle limit", time.Since(start), tc.idle)
			}
		})
	}
}

// Populate real cache entries before timing. Each measured write replaces a blob
// under budget, so a scan-based implementation exposes its O(entries) cost.
func BenchmarkStoreWrite(b *testing.B) {
	for _, count := range []int{10, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			dir := b.TempDir()
			for i := 0; i < count; i++ {
				data := []byte(fmt.Sprint(i))
				os.WriteFile(filepath.Join(dir, "cas-"+string(get(digest(data), 1).b)), data, 0600)
			}
			s, err := Open(dir, 1<<30)
			if err != nil {
				b.Fatal(err)
			}
			data := []byte("benchmark")
			d := digest(data)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.put("cas-", d, data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestUploadKeepsIncrementalHash(t *testing.T) {
	s, err := Open(t.TempDir(), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("x"), 3<<20)
	d := digest(data)
	name := "uploads/hash/blobs/" + string(get(d, 1).b) + fmt.Sprintf("/%d", len(data))
	partial := filepath.Base(s.uploadPath(name))
	var previous any
	for offset := 0; offset < len(data); offset += 16 << 10 {
		end := offset + (16 << 10)
		finish := end == len(data)
		if _, err = s.appendUpload(name, d, uint64(offset), data[offset:end], finish); err != nil {
			t.Fatal(err)
		}
		if !finish {
			s.mu.Lock()
			h := s.files[partial].Value.(*cacheEntry).hash
			s.mu.Unlock()
			if h == nil || (previous != nil && h != previous) {
				t.Fatal("incremental SHA-256 state replaced between chunks")
			}
			previous = h
		}
	}
	if !s.has(d) {
		t.Fatal("finished upload missing")
	}
}

// An expired HTTP/2 read timer can expose a plain error rather than net.Error.
// Its gRPC status must remain DEADLINE_EXCEEDED after a later timer refresh.
func TestOpaqueHTTP2Timeout(t *testing.T) {
	for _, err := range []error{os.ErrDeadlineExceeded, fmt.Errorf("i/o timeout"), fmt.Errorf("wrapped: %w", os.ErrDeadlineExceeded)} {
		if !grpcTimeout(err) {
			t.Errorf("deadline not recognized: %v", err)
		}
	}
	for _, err := range []error{nil, fmt.Errorf("malformed frame")} {
		if grpcTimeout(err) {
			t.Errorf("non-timeout recognized as deadline: %v", err)
		}
	}
}
