package reapi

// ActionResult, OutputFile, OutputDirectory, Tree, Directory and FileNode fields
// are from remote_execution.proto (see wire.go). Validate references on lookup:
// LRU eviction of CAS data must turn stale action results into cache misses.
func (s *Store) actionAvailable(result []byte) bool {
	if _, e := decode(result); e != nil {
		return false
	}
	for _, f := range all(result, 2) {
		if !s.has(get(f, 2).b) {
			return false
		}
	}
	for _, n := range []int{6, 8} {
		if d := get(result, n).b; d != nil && !s.has(d) {
			return false
		}
	}
	for _, out := range all(result, 3) {
		if d := get(out, 3).b; d != nil {
			tree, e := s.read("cas-", d)
			if e != nil {
				return false
			}
			dirs := append(all(tree, 1), all(tree, 2)...)
			for _, dir := range dirs {
				for _, f := range all(dir, 1) {
					if !s.has(get(f, 2).b) {
						return false
					}
				}
			}
		} else if d := get(out, 5).b; d != nil {
			if !s.directoryAvailable(d, map[string]bool{}) {
				return false
			}
		} else {
			return false
		}
	}
	return true
}
func (s *Store) directoryAvailable(d []byte, seen map[string]bool) bool {
	k, e := key(d)
	if e != nil {
		return false
	}
	if seen[k] {
		return true
	}
	seen[k] = true
	dir, e := s.read("cas-", d)
	if e != nil {
		return false
	}
	for _, f := range all(dir, 1) {
		if !s.has(get(f, 2).b) {
			return false
		}
	}
	for _, node := range all(dir, 2) {
		if !s.directoryAvailable(get(node, 2).b, seen) {
			return false
		}
	}
	return true
}
