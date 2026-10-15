package reapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/altdansalt/tun/internal/sandbox"
)

// LocalExecutor uses the CAS as its persistent input cache. Inputs are copied so
// commands cannot mutate cached bytes through hard links.
func LocalExecutor(store BlobStore, dataDir, mode string) func(context.Context, []byte, []byte) ([]byte, error) {
	return func(parent context.Context, action, command []byte) ([]byte, error) {
		timeout, err := actionTimeout(action)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		root, err := os.MkdirTemp(dataDir, "action-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(root)
		work, tmp := filepath.Join(root, "work"), filepath.Join(root, "tmp")
		if err = os.MkdirAll(work, 0700); err != nil {
			return nil, err
		}
		if err = os.MkdirAll(tmp, 0700); err != nil {
			return nil, err
		}
		fs, err := os.OpenRoot(work)
		if err != nil {
			return nil, err
		}
		defer fs.Close()
		if err = materialize(store, fs, ".", get(action, 2).b, 0); err != nil {
			return nil, err
		}
		cwd := string(get(command, 6).b)
		if err = safeRelative(cwd); err != nil {
			return nil, err
		}
		cwdRoot, err := fs.OpenRoot(cwd)
		if err != nil && cwd == "" {
			cwdRoot, err = fs.OpenRoot(".")
		}
		if err != nil {
			return nil, err
		}
		cwdRoot.Close()
		var argv, env []string
		for _, a := range all(command, 1) {
			argv = append(argv, string(a))
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("empty Command arguments")
		}
		for _, v := range all(command, 2) {
			k, val := string(get(v, 1).b), string(get(v, 2).b)
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(val, 0) {
				return nil, fmt.Errorf("invalid Command environment")
			}
			env = append(env, k+"="+val)
		}
		outputs := all(command, 7)
		if len(outputs) == 0 {
			outputs = append(all(command, 3), all(command, 4)...)
		}
		for _, out := range outputs {
			p := string(out)
			if err = safeRelative(p); err != nil {
				return nil, err
			}
			if err = fs.MkdirAll(filepath.Join(cwd, filepath.Dir(p)), 0755); err != nil {
				return nil, err
			}
		}
		var stdout, stderr bytes.Buffer
		cmd, net, err := sandbox.Start(sandbox.Spec{Mode: mode, Argv: argv, Env: env, Dir: filepath.Join(work, cwd), Root: root, Tmp: tmp, DataDir: dataDir, NetworkOff: true}, &stdout, &stderr)
		if err != nil {
			return nil, err
		}
		if net != nil {
			defer net.Close()
		}
		ended := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			case <-ended:
			}
		}()
		err = cmd.Wait()
		close(ended)
		code := int64(0)
		if err != nil {
			if ex, ok := err.(*exec.ExitError); ok {
				code = int64(ex.ExitCode())
			} else {
				return nil, err
			}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("action timeout or shutdown: %w", ctx.Err())
		}
		result := intField(4, code)
		for _, stream := range []struct {
			field int
			data  []byte
		}{{6, stdout.Bytes()}, {8, stderr.Bytes()}} {
			d := digest(stream.data)
			if err = store.put("cas-", d, stream.data); err != nil {
				return nil, err
			}
			result = append(result, bytesField(stream.field, d)...)
		}
		seen := map[string]bool{}
		for _, out := range outputs {
			p := string(out)
			if seen[p] {
				continue
			}
			seen[p] = true
			name := filepath.Join(cwd, p)
			info, err := fs.Lstat(name)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := fs.Readlink(name)
				if err != nil {
					return nil, err
				}
				symlinkField := 12
				if len(all(command, 7)) == 0 {
					symlinkField = 10
					for _, dir := range all(command, 4) {
						if string(dir) == p {
							symlinkField = 11
						}
					}
				}
				result = append(result, bytesField(symlinkField, msg(bytesField(1, []byte(p)), bytesField(2, []byte(target))))...)
			} else if info.IsDir() {
				dir, children, err := collectDirectory(store, fs, name)
				if err != nil {
					return nil, err
				}
				tree := bytesField(1, dir)
				for _, child := range children {
					tree = append(tree, bytesField(2, child)...)
				}
				d := digest(tree)
				if err = store.put("cas-", d, tree); err != nil {
					return nil, err
				}
				result = append(result, bytesField(3, msg(bytesField(1, []byte(p)), bytesField(3, d)))...)
			} else {
				node, err := uploadFile(store, fs, name, p, info)
				if err != nil {
					return nil, err
				}
				result = append(result, bytesField(2, node)...)
			}
		}
		return result, nil
	}
}
func nodeName(b []byte) (string, error) {
	n := string(get(b, 1).b)
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
		return "", fmt.Errorf("invalid Directory node name %q", n)
	}
	return n, nil
}
func materialize(s BlobStore, r *os.Root, p string, d []byte, depth int) error {
	if depth > 256 {
		return fmt.Errorf("input tree too deep")
	}
	b, err := s.read("cas-", d)
	if err != nil {
		return err
	}
	if _, err = decode(b); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, kind := range []int{1, 2, 3} {
		for _, node := range all(b, kind) {
			n, err := nodeName(node)
			if err != nil {
				return err
			}
			if names[n] {
				return fmt.Errorf("duplicate input %q", n)
			}
			names[n] = true
			name := filepath.Join(p, n)
			switch kind {
			case 1:
				data, err := s.read("cas-", get(node, 2).b)
				if err != nil {
					return err
				}
				mode := os.FileMode(0644)
				if get(node, 4).v != 0 {
					mode = 0755
				}
				f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
				if err != nil {
					return err
				}
				_, err = f.Write(data)
				ce := f.Close()
				if err != nil {
					return err
				}
				if ce != nil {
					return ce
				}
			case 2:
				if err = r.Mkdir(name, 0755); err != nil {
					return err
				}
				if err = materialize(s, r, name, get(node, 2).b, depth+1); err != nil {
					return err
				}
			case 3:
				target := string(get(node, 2).b)
				if err = r.Symlink(target, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func uploadFile(s BlobStore, r *os.Root, name, label string, info os.FileInfo) ([]byte, error) {
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("unsupported output %q", name)
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	d := digest(data)
	if err = s.put("cas-", d, data); err != nil {
		return nil, err
	}
	ex := int64(0)
	if info.Mode()&0111 != 0 {
		ex = 1
	}
	return msg(bytesField(1, []byte(label)), bytesField(2, d), intField(4, ex)), nil
}
func collectDirectory(s BlobStore, r *os.Root, p string) ([]byte, [][]byte, error) {
	f, err := r.Open(p)
	if err != nil {
		return nil, nil, err
	}
	ents, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, nil, err
	}
	// REAPI directories must be lexicographically sorted.
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var files, dirs, links []byte
	var children [][]byte
	for _, ent := range ents {
		name := filepath.Join(p, ent.Name())
		info, err := r.Lstat(name)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := r.Readlink(name)
			if err != nil {
				return nil, nil, err
			}
			links = append(links, bytesField(3, msg(bytesField(1, []byte(ent.Name())), bytesField(2, []byte(target))))...)
		} else if info.IsDir() {
			child, desc, err := collectDirectory(s, r, name)
			if err != nil {
				return nil, nil, err
			}
			d := digest(child)
			if err = s.put("cas-", d, child); err != nil {
				return nil, nil, err
			}
			dirs = append(dirs, bytesField(2, msg(bytesField(1, []byte(ent.Name())), bytesField(2, d)))...)
			children = append(children, child)
			children = append(children, desc...)
		} else {
			node, err := uploadFile(s, r, name, ent.Name(), info)
			if err != nil {
				return nil, nil, err
			}
			files = append(files, bytesField(1, node)...)
		}
	}
	return msg(files, dirs, links), children, nil
}

// BlobStore is the executor's CAS interface. Remote executors retain verified
// input bytes in a local Store before materializing an action.
type BlobStore interface {
	read(string, []byte) ([]byte, error)
	put(string, []byte, []byte) error
}
type RemoteStore struct {
	Local  *Store
	Fetch  func([]byte) ([]byte, error)
	Upload func([]byte, []byte) error
}

func (s *RemoteStore) read(prefix string, d []byte) ([]byte, error) {
	if b, err := s.Local.read(prefix, d); err == nil {
		return b, nil
	}
	b, err := s.Fetch(d)
	if err != nil {
		return nil, err
	}
	if err = s.Local.put(prefix, d, b); err != nil {
		return nil, err
	}
	return b, nil
}
func (s *RemoteStore) put(prefix string, d, b []byte) error { return s.Upload(d, b) }

// ReadBlob and PutBlob are used by the authenticated runner transport.
func (s *Store) ReadBlob(d []byte) ([]byte, error) { return s.read("cas-", d) }
func (s *Store) PutBlob(d, b []byte) error         { return s.put("cas-", d, b) }
