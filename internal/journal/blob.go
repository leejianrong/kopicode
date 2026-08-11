package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BlobsSubdir holds spilled payload content, under StateDir.
//
// It sits beside sessions/ rather than inside a session directory: blobs are
// named by their content, so two sessions that observe the same 300 KiB of test
// output store it once. Nothing in a blob's name says which session put it
// there, and nothing needs to.
const BlobsSubdir = "blobs"

// DefaultBlobThreshold is the payload-field size above which content is written
// to a blob instead of into the event line.
//
// 64 KiB is arbitrary and tunable (docs/SLICE-1.md §1) — WithBlobThreshold
// changes it per journal. What is not tunable is that nothing is clipped: a
// field over the threshold moves, it does not shrink.
const DefaultBlobThreshold = 64 << 10

// Sentinel causes for the blob store, for errors.Is.
var (
	// ErrBlobMissing reports an event referencing a blob that is not on disk.
	ErrBlobMissing = errors.New("blob is missing")
	// ErrBlobCorrupt reports a blob whose content does not hash to its own
	// name. The name is the checksum, so this is detectable rather than
	// merely suspected.
	ErrBlobCorrupt = errors.New("blob content does not match its name")
	// ErrBlobRefInvalid reports a reference that is not a sha-256 digest at
	// all. It is checked before the name is joined to a path, so a journal
	// line saying "../../../etc/passwd" reads a blob that does not exist
	// rather than a file that does.
	ErrBlobRefInvalid = errors.New("blob ref is not a sha-256 digest")
)

// blobStore is the content-addressed store under .kopicode/blobs.
//
// A blob's name is the sha-256 of its bytes, hex-encoded, which buys three
// things at once: identical content written twice is one file, a reference is
// self-verifying, and there is no allocator, no free list and no ordering to get
// wrong. Nothing here mutates or deletes; a blob is written once and then only
// read.
type blobStore struct {
	dir string
}

// blobPath resolves a reference, refusing anything that is not a digest.
func (s *blobStore) blobPath(ref string) (string, error) {
	if !validBlobRef(ref) {
		return "", fmt.Errorf("journal: %w: %q", ErrBlobRefInvalid, ref)
	}
	return filepath.Join(s.dir, ref), nil
}

// validBlobRef reports whether ref is 64 lowercase hex characters — the only
// shape this package writes, and therefore the only shape it will follow.
func validBlobRef(ref string) bool {
	if len(ref) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// put stores content and returns its digest.
//
// # Atomicity
//
// The write goes to a temp file in the same directory, is fsynced, and is then
// renamed into place; the directory is fsynced after. A name in this store
// therefore never refers to a half-written file. That matters more here than
// for an ordinary cache: the name *claims* to be the content's checksum, so a
// truncated blob under a complete name is not a missing file, it is a lie that
// survives restarts and that every later read has to disprove.
//
// # Durability ordering
//
// put returns only once the blob is durable, and Append calls it before writing
// the event line that references it. The two failure directions are not
// symmetric: a blob with no event pointing at it is unreferenced garbage, while
// an event pointing at a blob that did not survive the crash is a record
// claiming data it cannot produce.
//
// Content that is already stored is not rewritten. Because writes are atomic,
// an existing name is a complete blob, so re-verifying it on every put would
// buy nothing but a full read of every blob in the store.
func (s *blobStore) put(content []byte) (string, error) {
	sum := sha256.Sum256(content)
	ref := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, ref)

	if _, err := os.Stat(path); err == nil {
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("journal: checking blob %s: %w", path, err)
	}

	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return "", fmt.Errorf("journal: creating blob directory %s: %w", s.dir, err)
	}

	// CreateTemp opens 0600, which is the mode a blob keeps: it holds whatever
	// the model read, and that is as private as the events file.
	tmp, err := os.CreateTemp(s.dir, "spill-*.tmp")
	if err != nil {
		return "", fmt.Errorf("journal: creating a temp file for blob %s: %w", ref, err)
	}
	tmpName := tmp.Name()
	// Every failure below leaves the temp file behind; remove it rather than
	// litter the store with debris a later reader cannot interpret.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("journal: writing blob %s: %w", ref, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("journal: fsync of blob %s: %w", ref, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("journal: closing blob %s: %w", ref, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("journal: publishing blob %s: %w", ref, err)
	}
	// The rename is a directory modification and is not durable until the
	// directory is fsynced — same reason Open fsyncs the session directory.
	if err := syncDir(s.dir); err != nil {
		return "", err
	}
	return ref, nil
}

// get returns a blob's content, verified against its own name.
//
// The check is not paranoia about disks. The name is a checksum this package
// computed, so verifying it costs one hash over content the caller is about to
// read anyway, and it converts "the reviewer is reading output that is not what
// the tool produced" — which is undetectable — into a named error.
func (s *blobStore) get(ref string) ([]byte, error) {
	path, err := s.blobPath(ref)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("journal: %s: %w", path, ErrBlobMissing)
		}
		return nil, fmt.Errorf("journal: reading blob %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != ref {
		return nil, fmt.Errorf("journal: %s: %w: %d bytes hash to %s", path, ErrBlobCorrupt, len(content), got)
	}
	return content, nil
}

// BlobDir is where spilled content lives under root, so callers agree on the
// layout instead of each re-deriving it.
func BlobDir(root string) string {
	return filepath.Join(root, StateDir, BlobsSubdir)
}
