package server

// Session artifacts are snapshotted before releasing the session lock. The tar is
// shipped to the hub just like v0 outputs, so downloads survive eviction/restarts.
import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/altdansalt/tun/internal/bep"
)

const artifactIndex = ".tun-artifacts.json"

func (s *Server) stageSessionArtifacts(j *job, out string, sum bep.Summary) error {
	root, err := os.OpenRoot(out)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := os.CreateTemp(s.jobDir(j.ID), ".artifacts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	tw := tar.NewWriter(f)
	index := map[string][]string{}
	seen := map[string]bool{}
	var total int64
	add := func(label, name, source string) error {
		if source == "" {
			return nil
		}
		source = filepath.Clean(source)
		if strings.HasPrefix(source, "/tun/out/") {
			source = strings.TrimPrefix(source, "/tun/out/")
		} else if strings.HasPrefix(source, out+"/") {
			source = strings.TrimPrefix(source, out+"/")
		} else {
			return fmt.Errorf("artifact outside session output base: %s", source)
		}
		if !filepath.IsLocal(source) || !filepath.IsLocal(name) {
			return fmt.Errorf("invalid artifact path")
		}
		in, err := root.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		fi, err := in.Stat()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("artifact is not a regular file: %s", source)
		}
		index[label] = append(index[label], name)
		if seen[name] {
			return nil
		}
		seen[name] = true
		total += fi.Size()
		if total > s.maxDownload() {
			return fmt.Errorf("session artifacts exceed download limit")
		}
		h, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		h.Name = name
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err = io.CopyN(tw, in, fi.Size())
		return err
	}
	// Bazel's workspace symlink may be absolute in its mount namespace. Translate
	// that link, then open only through the output base's os.Root.
	work := filepath.Join(s.sessionDir(j.SessionID), "sandbox", "work")
	for _, t := range sum.Targets {
		for _, a := range t.Outputs {
			prefix, rest, ok := strings.Cut(a.Path, "/")
			if !ok {
				continue
			}
			link, err := os.Readlink(filepath.Join(work, prefix))
			if err != nil {
				continue
			}
			if !filepath.IsAbs(link) {
				link = filepath.Clean(filepath.Join(work, link))
			}
			if err := add(t.Label, a.Path, filepath.Join(link, rest)); err != nil {
				return err
			}
		}
	}
	for _, t := range sum.Tests {
		// A test label fetches its logs as well as declared outputs; .log is an alias.
		for _, a := range t.Attempts {
			for i, log := range a.Logs {
				name := fmt.Sprintf("bazel-testlogs/%s/run_%d_shard_%d_attempt_%d/%d-%s", strings.ReplaceAll(strings.TrimPrefix(t.Label, "//"), ":", "/"), a.Run, a.Shard, a.Attempt, i, filepath.Base(log.Path))
				if err := add(t.Label, name, log.Path); err != nil {
					return err
				}
				index[t.Label+".log"] = append(index[t.Label+".log"], name)
			}
		}
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: artifactIndex, Mode: 0600, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.jobDir(j.ID), outputsFile))
}

// resolveSessionPaths reads the uncapped label index retained with this command.
func resolveSessionPaths(f *os.File, paths []string) ([]string, error) {
	if f == nil {
		return paths, nil
	}
	defer f.Seek(0, io.SeekStart)
	tr := tar.NewReader(f)
	index := map[string][]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Name == artifactIndex {
			if err := json.NewDecoder(io.LimitReader(tr, maxBEP)).Decode(&index); err != nil {
				return nil, err
			}
			break
		}
	}
	var result []string
	for _, p := range paths {
		if strings.HasPrefix(p, "//") {
			if names := index[p]; len(names) > 0 {
				result = append(result, names...)
			} else {
				result = append(result, p)
			}
		} else {
			result = append(result, p)
		}
	}
	return result, nil
}
