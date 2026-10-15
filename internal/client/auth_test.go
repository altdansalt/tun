package client

import (
	"net/http"
	"testing"

	"github.com/altdansalt/tun/internal/server"
)

// Proxies in front of tun (exe.dev's) consume the Authorization header, so the token
// must also work as X-Tun-Token, and a wrong X-Tun-Token must not fall back to Authorization.
func TestAuthHeaders(t *testing.T) {
	ts := startServer(t, server.Config{})
	tok := ts.srv.Token()
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"none", nil, 401},
		{"bearer", map[string]string{"Authorization": "Bearer " + tok}, 200},
		{"x-tun-token", map[string]string{"X-Tun-Token": tok}, 200},
		{"wrong x-tun-token", map[string]string{"X-Tun-Token": "nope"}, 401},
		{"wrong x-tun-token, right bearer", map[string]string{"X-Tun-Token": "nope", "Authorization": "Bearer " + tok}, 401},
		{"wrong bearer", map[string]string{"Authorization": "Bearer nope"}, 401},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest("GET", ts.url+"/v1/jobs?limit=1", nil)
		for k, v := range tc.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}
