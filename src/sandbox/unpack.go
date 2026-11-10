package sandbox

import (
	"errors"
	"io"
	"sync"
)

// what the tar readers share: a registry's layers (bwrap_image.go) and the
// tar an lxd instance writes (lxd.go) are both unpacked on the host, and
// neither archive is pats's own. each is written through an os.Root of the
// directory it unpacks into, which follows a symlink only to where it stays
// inside that directory, so no entry is written through one that leaves it.

// errEscape is an archive entry whose own name or link names a place outside
// the directory it is unpacked into.
var errEscape = errors.New("leaves the destination")

// maxPath is the longest name an entry may have: linux's PATH_MAX. a root
// opens a path one component at a time and so takes one of any length, at a
// cost that grows with the square of its depth, and leaves a tree few tools
// can remove.
const maxPath = 4096

// copyBuf lends the buffer a file is copied through. io.Copy makes one of
// 32 KiB for every file it copies between a file and what is no file.
var copyBuf = sync.Pool{New: func() any { return new([32 << 10]byte) }}

// copyFileBytes is io.Copy through a lent buffer.
func copyFileBytes(dst io.Writer, src io.Reader) (int64, error) {
	buf := copyBuf.Get().(*[32 << 10]byte)
	defer copyBuf.Put(buf)
	// the bare interfaces: a file's own ReadFrom and WriteTo fall back to
	// io.Copy and its buffer.
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, buf[:])
}

var errLong = errors.New("is longer than a path may be")
