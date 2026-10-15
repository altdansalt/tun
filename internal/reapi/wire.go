// Package reapi implements the supported subset of REAPI using the protobuf wire format.
package reapi

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Field numbers follow https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto
// and https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto.
// google.rpc.Status uses code=1, message=2:
// https://github.com/googleapis/googleapis/blob/master/google/rpc/status.proto
// SemVer uses major=1, minor=2, patch=3:
// https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/semver/semver.proto
// Unknown fields are skipped, preserving opaque ActionResult messages verbatim.
type field struct {
	n int
	w uint64
	v uint64
	b []byte
}

func decode(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 || tag>>3 > (1<<29)-1 {
			return nil, fmt.Errorf("invalid protobuf tag")
		}
		b = b[n:]
		f := field{n: int(tag >> 3), w: tag & 7}
		switch tag & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("invalid varint")
			}
			f.v = v
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, fmt.Errorf("short fixed64")
			}
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return nil, fmt.Errorf("short bytes")
			}
			f.b = b[n : n+int(l)]
			b = b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, fmt.Errorf("short fixed32")
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("unsupported wire type")
		}
		out = append(out, f)
	}
	return out, nil
}
func bytesField(n int, b []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(n<<3|2))
	out = binary.AppendUvarint(out, uint64(len(b)))
	return append(out, b...)
}
func intField(n int, v int64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(n<<3)), uint64(v))
}
func get(b []byte, n int) field {
	fs, _ := decode(b)
	for i := len(fs) - 1; i >= 0; i-- {
		if fs[i].n == n {
			return fs[i]
		}
	}
	return field{}
}
func all(b []byte, n int) [][]byte {
	fs, _ := decode(b)
	var out [][]byte
	for _, f := range fs {
		if f.n == n {
			out = append(out, f.b)
		}
	}
	return out
}
func msg(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// Digest is remote_execution.proto Digest: hash=1, size_bytes=2.
// SHA256 is the only digest function advertised by this implementation.
type Digest struct {
	Hash string
	Size int64
}

func parseDigest(b []byte) (Digest, error) {
	fs, e := decode(b)
	if e != nil {
		return Digest{}, e
	}
	var d Digest
	for _, f := range fs {
		switch f.n {
		case 1:
			if f.w != 2 {
				return d, fmt.Errorf("digest hash must be bytes")
			}
			d.Hash = string(f.b)
		case 2:
			if f.w != 0 || f.v > 1<<63-1 {
				return d, fmt.Errorf("invalid digest size")
			}
			d.Size = int64(f.v)
		}
	}
	raw, e := hex.DecodeString(d.Hash)
	if e != nil || len(raw) != 32 || hex.EncodeToString(raw) != d.Hash {
		return d, fmt.Errorf("invalid SHA256 digest")
	}
	return d, nil
}
func (d Digest) marshal() []byte { return msg(bytesField(1, []byte(d.Hash)), intField(2, d.Size)) }
