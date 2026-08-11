package corpus

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// digestPrefix labels the hash so a future change of algorithm is visible in
// every recorded result rather than silently producing a different-looking
// string.
const digestPrefix = "sha256:"

// Digest returns a content digest over the task tree at root.
//
// It is what makes "frozen" checkable. ADR-0005 requires the corpus to be
// frozen and versioned for an experiment series, and a version string on its
// own only records an intention: it stays the same whether or not somebody
// edited a task. The digest changes the moment any task file does, so a result
// that records both says exactly which bytes produced it.
//
// What is covered: every file under root, at any depth, except Markdown and
// except corpus.json itself. Markdown is excluded because corpus documentation
// is not part of what the agent is measured on, and correcting a typo in a
// README should not invalidate an experiment series. corpus.json is excluded
// because it is where the digest is recorded.
//
// The hash is over length-prefixed (path, content) pairs in sorted path order,
// with forward slashes, so it is identical on every platform and cannot be
// changed by moving a byte from one file's name into the next file's contents.
func Digest(root string) (string, error) {
	type entry struct {
		rel  string
		path string
	}

	var files []entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName || strings.HasSuffix(strings.ToLower(rel), ".md") {
			return nil
		}
		files = append(files, entry{rel: rel, path: path})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("corpus: walking %s: %w", root, err)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	h := sha256.New()
	var length [8]byte
	write := func(b []byte) {
		binary.BigEndian.PutUint64(length[:], uint64(len(b)))
		h.Write(length[:])
		h.Write(b)
	}

	for _, f := range files {
		content, err := os.ReadFile(f.path)
		if err != nil {
			return "", fmt.Errorf("corpus: reading %s: %w", f.rel, err)
		}
		write([]byte(f.rel))
		write(content)
	}

	return digestPrefix + hex.EncodeToString(h.Sum(nil)), nil
}
