package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type identityKey struct{}
type principal struct {
	name  string
	admin bool
}

func (s *Server) identity(r *http.Request) (string, bool, bool) {
	token := r.Header.Get("X-Tun-Token")
	if token == "" {
		token, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1 {
		return clientOf(r), true, true
	}
	sum := sha256.Sum256([]byte(token))
	b, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "clients", hex.EncodeToString(sum[:])))
	name := string(b)
	return name, false, token != "" && err == nil && name != "" && cleanName(name) == name
}
func (s *Server) authenticate(r *http.Request) bool    { _, _, ok := s.identity(r); return ok }
func (s *Server) cacheIdentity(r *http.Request) string { name, _, _ := s.identity(r); return name }

// IssueClientToken stores only the token hash; multiple tokens may name the same client.
// Removing its hash file revokes a token immediately, without restarting the server.
func IssueClientToken(data, name string) (string, error) {
	if name == "" || cleanName(name) != name {
		return "", fmt.Errorf("client name must be 1-128 printable non-space ASCII characters")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	dir := filepath.Join(data, "clients")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])), []byte(name), 0600)
	return token, err
}

func TokenMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", DefaultDataDir(), "server data directory (run on server host)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tun token [--data DIR] new NAME\nPrint a client token; store its SHA256 hash in DATA/clients. Headers cannot change its identity.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 125
	}
	if fs.NArg() != 2 || fs.Arg(0) != "new" {
		fs.Usage()
		return 125
	}
	token, err := IssueClientToken(*data, fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, "tun:", err)
		return 125
	}
	fmt.Fprintln(stdout, token)
	return 0
}
