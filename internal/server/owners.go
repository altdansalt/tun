package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Uploaded content (workspace blobs and tun bazel's CAS) is stored once, but a client can
// only use, or learn the existence of, content it uploaded itself. A digest is not a
// capability: job metadata, logs or a guess of low-entropy content (a config file with a
// short secret) can reveal it, so proof of possession is the upload. An upload of content
// another client already uploaded is verified, then adds the uploader as an owner instead
// of storing a second copy.
//
// Ownership is a hard link to the shared file: owners/<client key>/<kind>/<sha>. Checking it
// is one stat; it costs no extra data or inode. The sweep that deletes shared files also
// deletes links to them, so links never keep deleted content alive.

// owner is whose ownership a request uses: "" for admin, which may use any content.
func owner(r *http.Request) string {
	if p, ok := r.Context().Value(identityKey{}).(principal); ok && !p.admin {
		return p.name
	}
	return ""
}

func (s *Server) ownerDir(client, kind string) string {
	h := sha256.Sum256([]byte(client))
	return filepath.Join(s.cfg.DataDir, "owners", hex.EncodeToString(h[:16]), kind)
}

// owns reports whether client uploaded shared file path (named sha in kind), and if so
// marks it used. A link to an older, replaced copy doesn't count (and is removed). Caller
// holds s.gc.blobMu.
func (s *Server) owns(client, kind, sha, path string) bool {
	if client == "" {
		now := time.Now()
		return os.Chtimes(path, now, now) == nil
	}
	link := filepath.Join(s.ownerDir(client, kind), sha)
	li, err := os.Lstat(link)
	if err != nil {
		return false
	}
	fi, err := os.Lstat(path)
	if err != nil || !os.SameFile(li, fi) {
		os.Remove(link)
		return false
	}
	now := time.Now()
	return os.Chtimes(path, now, now) == nil
}

// store moves verified upload tmp to the shared path (keeping an existing copy, so other
// owners' links stay valid) and records client as an owner. It takes s.gc.blobMu.
func (s *Server) store(client, kind, sha, tmp, path string) error {
	s.gc.blobMu.Lock()
	defer s.gc.blobMu.Unlock()
	if _, err := os.Lstat(path); err == nil {
		os.Remove(tmp)
		now := time.Now()
		os.Chtimes(path, now, now)
	} else if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if client == "" {
		return nil
	}
	dir := s.ownerDir(client, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	link := filepath.Join(dir, sha)
	os.Remove(link) // possibly a link to a replaced copy
	return os.Link(path, link)
}

// sweepOwners deletes ownership links whose shared file (at path(sha)) is gone or was
// replaced. Caller holds s.gc.blobMu.
func (s *Server) sweepOwners(kind string, path func(sha string) string) {
	clients, _ := os.ReadDir(filepath.Join(s.cfg.DataDir, "owners"))
	for _, c := range clients {
		dir := filepath.Join(s.cfg.DataDir, "owners", c.Name(), kind)
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			li, err := e.Info()
			if err != nil {
				continue
			}
			if fi, err := os.Lstat(path(e.Name())); err != nil || !os.SameFile(li, fi) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}
