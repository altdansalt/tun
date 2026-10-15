package reapi

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
)

// WebTransport bridges native gRPC to binary gRPC-Web. Request frames are
// identical and stream unchanged; response trailer frames become HTTP trailers.
// It never buffers an entire RPC or blob.
type WebTransport struct{ Base http.RoundTripper }

func (t WebTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Content-Type", "application/grpc-web+proto")
	r.Header.Del("Te")
	resp, err := t.Base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	if resp.Header.Get("Content-Type") != "application/grpc-web+proto" {
		resp.Body.Close()
		return nil, fmt.Errorf("gRPC-Web upstream: HTTP %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp.Header.Set("Content-Type", "application/grpc")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Trailer = http.Header{"Grpc-Status": nil, "Grpc-Message": nil}
	resp.Body = &webBody{ReadCloser: resp.Body, trailer: resp.Trailer}
	return resp, nil
}

type webBody struct {
	io.ReadCloser
	trailer   http.Header
	pending   []byte
	remaining int64
	done      bool
}

func (b *webBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	if b.done {
		return 0, io.EOF
	}
	if b.remaining > 0 {
		if int64(len(p)) > b.remaining {
			p = p[:b.remaining]
		}
		n, err := b.ReadCloser.Read(p)
		b.remaining -= int64(n)
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	var h [5]byte
	if _, err := io.ReadFull(b.ReadCloser, h[:]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	size := int64(binary.BigEndian.Uint32(h[1:]))
	if h[0] == 0 && size <= maxMessage {
		b.pending = append([]byte(nil), h[:]...)
		b.remaining = size
		return b.Read(p)
	}
	if h[0] != 0x80 || size > 64<<10 {
		return 0, fmt.Errorf("invalid gRPC-Web frame")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(b.ReadCloser, data); err != nil {
		return 0, err
	}
	headers, err := textproto.NewReader(bufio.NewReader(strings.NewReader(string(data) + "\r\n"))).ReadMIMEHeader()
	if err != nil {
		return 0, err
	}
	if headers.Get("Grpc-Status") == "" {
		return 0, fmt.Errorf("missing gRPC-Web status")
	}
	for _, key := range []string{"Grpc-Status", "Grpc-Message"} {
		b.trailer[key] = headers.Values(key)
	}
	var extra [1]byte
	if n, err := b.ReadCloser.Read(extra[:]); n != 0 || err != io.EOF {
		return 0, fmt.Errorf("data after gRPC-Web trailers")
	}
	b.done = true
	return 0, io.EOF
}
