package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A used transport has "h2" in its TLS NextProtos; its HTTP/1 clone must still negotiate
// HTTP/1.1 with an h2-capable TLS server.
func TestHTTP1TransportAfterH2Use(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	base := srv.Client().Transport.(*http.Transport).Clone()
	base.ForceAttemptHTTP2 = true
	resp, err := (&http.Client{Transport: base}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("base used %s, want HTTP/2 so its TLS config carries h2", resp.Proto)
	}
	tr := http1Transport(base)
	defer tr.CloseIdleConnections()
	resp, err = (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 1 || string(b) != "HTTP/1.1" {
		t.Fatalf("got %s / %q, want HTTP/1.1", resp.Proto, b)
	}
}
