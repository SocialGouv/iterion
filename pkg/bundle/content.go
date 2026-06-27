package bundle

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
)

// contentHasher computes a stable, format-independent SHA-256 over the
// LOGICAL content of a bundle: the sorted sequence of (relative-path,
// file-bytes) pairs. It deliberately ignores the container format (ZIP
// vs tar.gz), directory entries, file modes, and timestamps — only the
// file paths and their bytes matter for change-detection and integrity.
//
// Because both the writer (PackDir) and the loader (collectContentHash)
// feed files in the SAME sorted order through this hasher, a given set of
// files yields the same hash whether it was freshly packed as a ZIP or
// read back from a legacy tar.gz bundle.
//
// Each file contributes two length-prefixed records — its path then its
// full body, each framed with a 1-byte tag and a 64-bit big-endian byte
// count. The full body is framed as ONE record (independent of how it was
// streamed/chunked), so the digest never depends on read-buffer
// boundaries. Length-prefixing makes the encoding unambiguous: {"ab","c"}
// cannot collide with {"a","bc"}.
type contentHasher struct {
	h   hash.Hash
	buf []byte // bytes of the file currently being streamed
	cur bool   // a file is open (AddFile called, body not yet flushed)
}

const (
	recordPath byte = 'P'
	recordBody byte = 'B'
)

func newContentHasher() *contentHasher {
	return &contentHasher{h: sha256.New()}
}

// AddFile opens a new logical file record. It MUST be called once,
// immediately before the file's bytes are written through the hasher,
// for every regular file in deterministic (sorted) order. Directories
// are not hashed. Any previously-open file is flushed first.
func (c *contentHasher) AddFile(rel string) {
	c.flush()
	c.writeRecord(recordPath, []byte(rel))
	c.buf = c.buf[:0]
	c.cur = true
}

// Write buffers a chunk of the current file's body. It satisfies
// io.Writer so the body can be streamed via io.MultiWriter / io.Copy.
// The buffered bytes are folded into the hash as a single body record
// when the next file opens (AddFile) or when Sum is called.
func (c *contentHasher) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// flush emits the buffered body of the currently-open file (if any) as a
// single length-prefixed record.
func (c *contentHasher) flush() {
	if !c.cur {
		return
	}
	c.writeRecord(recordBody, c.buf)
	c.buf = c.buf[:0]
	c.cur = false
}

func (c *contentHasher) writeRecord(tag byte, b []byte) {
	var lenbuf [8]byte
	binary.BigEndian.PutUint64(lenbuf[:], uint64(len(b)))
	c.h.Write([]byte{tag})
	c.h.Write(lenbuf[:])
	c.h.Write(b)
}

// Sum flushes any open file body and returns the final digest. nil is
// the conventional argument.
func (c *contentHasher) Sum(b []byte) []byte {
	c.flush()
	return c.h.Sum(b)
}
