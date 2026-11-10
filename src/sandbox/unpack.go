package sandbox

import "errors"

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

var errLong = errors.New("is longer than a path may be")
