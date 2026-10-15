package api

import (
	"fmt"
	"path"
	"strings"
)

// Response headers of GET /v1/jobs/{id}/files (a tar stream). Each value is
// url.PathEscape'd; the headers repeat, one per item.
const (
	HeaderMissing = "X-Tun-Missing" // a requested path or glob that matched nothing
	HeaderSkipped = "X-Tun-Skipped" // "path: reason" for an entry left out (unsafe symlink, special file)
)

// ValidFilePath checks a path or path.Match glob for a files request: relative to the
// workspace root, without "..", NUL or backslashes.
func ValidFilePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("path %q must be relative to the workspace root", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("path %q must not contain ..", p)
		}
	}
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("path %q: bad glob pattern", p)
	}
	return nil
}
