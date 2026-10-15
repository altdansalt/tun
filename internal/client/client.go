// Package client implements the tun client: a small HTTP API client and the CLI commands
// run, status, logs, cancel and ls.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// Client talks to a tun server.
type Client struct {
	URL   string // e.g. http://127.0.0.1:7070
	Token string
	HTTP  *http.Client
}

// StatusError is a non-2xx response from the server.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	if e.Code == 401 {
		return "unauthorized: " + e.Msg + "; set $TUN_TOKEN or put the token in ~/.config/tun/token"
	}
	return fmt.Sprintf("server: %s (HTTP %d)", e.Msg, e.Code)
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Tun-Token", c.Token) // survives proxies that consume Authorization
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cannot reach tun server at %s (is `tun serve` running? set $TUN_URL): %v", c.URL, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e api.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
			if e.Error == "" {
				e.Error = resp.Status
			}
		}
		return nil, &StatusError{resp.StatusCode, e.Error}
	}
	return resp, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	var size int64
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, size = strings.NewReader(string(b)), int64(len(b))
	}
	resp, err := c.request(ctx, method, path, body, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("bad response from server: %v", err)
	}
	return nil
}

// HasBlob reports whether the server already has the blob.
func (c *Client) HasBlob(ctx context.Context, sha string) (bool, error) {
	resp, err := c.request(ctx, "HEAD", "/v1/blobs/"+sha, nil, 0)
	var se *StatusError
	if errors.As(err, &se) && se.Code == 404 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return true, nil
}

// PutBlob uploads the file at path as blob sha.
func (c *Client) PutBlob(ctx context.Context, sha, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, "PUT", "/v1/blobs/"+sha, f, fi.Size())
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) Create(ctx context.Context, req api.CreateJob) (api.Job, error) {
	var j api.Job
	err := c.doJSON(ctx, "POST", "/v1/jobs", req, &j)
	return j, err
}

func (c *Client) Get(ctx context.Context, id string) (api.Job, error) {
	var j api.Job
	err := c.doJSON(ctx, "GET", "/v1/jobs/"+id, nil, &j)
	return j, err
}

func (c *Client) List(ctx context.Context, limit int) ([]api.Job, error) {
	var js []api.Job
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/v1/jobs?limit=%d", limit), nil, &js)
	return js, err
}

func (c *Client) Cancel(ctx context.Context, id string) (api.Job, error) {
	var j api.Job
	err := c.doJSON(ctx, "POST", "/v1/jobs/"+id+"/cancel", nil, &j)
	return j, err
}

// Logs copies a job's stdout or stderr to w starting at offset. With follow it streams until
// the job has ended and every byte is written, reconnecting (from the current offset) if the
// connection drops. It returns the number of bytes written.
func (c *Client) Logs(ctx context.Context, id, stream string, offset int64, follow bool, w io.Writer) (int64, error) {
	start := offset
	failures := 0
	for {
		q := fmt.Sprintf("/v1/jobs/%s/%s?offset=%d", id, stream, offset)
		if follow {
			q += "&follow=1"
		}
		resp, err := c.request(ctx, "GET", q, nil, 0)
		if err == nil {
			var n int64
			n, err = io.Copy(w, resp.Body)
			resp.Body.Close()
			offset += n
			if err == nil {
				return offset - start, nil // server ended the stream: job done (or not following)
			}
			if n > 0 {
				failures = 0
			}
		}
		var se *StatusError
		if ctx.Err() != nil || errors.As(err, &se) || !follow {
			return offset - start, err
		}
		// Transient (server restart, network blip): retry for ~30s.
		if failures++; failures > 30 {
			return offset - start, fmt.Errorf("lost connection while following %s of job %s: %v", stream, id, err)
		}
		select {
		case <-ctx.Done():
			return offset - start, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// LogsTail copies the last n lines of a job's stdout or stderr to w; with follow it then
// streams the rest like Logs until the job ends.
func (c *Client) LogsTail(ctx context.Context, id, stream string, n int, follow bool, w io.Writer) error {
	resp, err := c.request(ctx, "GET", fmt.Sprintf("/v1/jobs/%s/%s?tail=%d", id, stream, n), nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	start, err := strconv.ParseInt(resp.Header.Get("X-Tun-Offset"), 10, 64)
	if err != nil {
		return fmt.Errorf("server did not report a log offset for --tail (is it older than this client?)")
	}
	k, err := io.Copy(w, resp.Body)
	if err != nil || !follow {
		return err
	}
	_, err = c.Logs(ctx, id, stream, start+k, true, w)
	return err
}
