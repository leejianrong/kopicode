package journal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/journal"
)

// blobNames lists the blob store's contents, and fails if anything in it is not
// a finished blob. A temp file left behind by an interrupted write would be
// invisible to every other assertion here, and it is exactly what the atomic
// rename exists to prevent.
func blobNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(journal.BlobDir(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the blob directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file survived in the blob store: %s — a write was not atomic", e.Name())
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

func readBlob(t *testing.T, root, ref string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(journal.BlobDir(root), ref))
	if err != nil {
		t.Fatalf("reading blob %s: %v", ref, err)
	}
	return b
}

// collectAll drains Read completely, keeping every event and every error. The
// truncated-tail helper in file_test.go stops at the first error, which is the
// right shape for a failure that ends the record; a damaged blob does not end
// it, so this one keeps going.
func collectAll(t *testing.T, j *journal.FileJournal) ([]journal.Event, []error) {
	t.Helper()
	var events []journal.Event
	var errs []error
	for ev, err := range j.Read(context.Background()) {
		events = append(events, ev)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return events, errs
}

func output(t *testing.T, ev journal.Event) journal.Text {
	t.Helper()
	result, ok := ev.Payload.(journal.ToolResult)
	if !ok {
		t.Fatalf("payload is %T, want journal.ToolResult", ev.Payload)
	}
	return result.Output
}

// TestSpillIsTransparentAndLosesNothing is the card's headline: a caller
// appends a value and reads the same value back, whichever side of the
// threshold it falls on.
//
// The boundary cases are the point. "Over 64 KiB spills" is easy to write as
// >=, as >, or against the encoded length rather than the value's, and all
// three pass a test that only tries 1 byte and 1 MiB.
func TestSpillIsTransparentAndLosesNothing(t *testing.T) {
	const threshold = 4096

	cases := map[string]struct {
		size        int
		wantSpilled bool
	}{
		"empty":              {size: 0, wantSpilled: false},
		"one under":          {size: threshold - 1, wantSpilled: false},
		"exactly at":         {size: threshold, wantSpilled: false},
		"one over":           {size: threshold + 1, wantSpilled: true},
		"far over":           {size: 40 * threshold, wantSpilled: true},
		"far over, unicode":  {size: 9000, wantSpilled: true},
		"far over, newlines": {size: 12000, wantSpilled: true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			j := open(t, root, journal.WithBlobThreshold(threshold))

			// Content that is not a repeated byte, so a partial write or an
			// off-by-one at either end shows up as a mismatch rather than as
			// more of the same character.
			var sb strings.Builder
			for sb.Len() < tc.size {
				fmt.Fprintf(&sb, "%d: диагностика & <output> \"quoted\"\n", sb.Len())
			}
			content := sb.String()[:tc.size]

			ev, err := j.Append(context.Background(), 1, journal.ToolResult{
				CallID: "call_1", Tool: "run_shell", Output: journal.InlineText(content),
			})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}

			// Append returns the event as written.
			written := output(t, ev)
			if written.Spilled() != tc.wantSpilled {
				t.Fatalf("Append returned Spilled() = %v, want %v for %d bytes at threshold %d",
					written.Spilled(), tc.wantSpilled, tc.size, threshold)
			}
			if written.Size != int64(tc.size) {
				t.Errorf("Size = %d, want %d — a reader must know what it is holding without fetching",
					written.Size, tc.size)
			}
			if tc.wantSpilled && written.Inline != "" {
				t.Errorf("the written event still carries %d inline bytes beside its ref", len(written.Inline))
			}

			// The line itself holds the ref, not the content.
			raw := readRaw(t, root)
			switch {
			case tc.wantSpilled:
				if bytes.Contains(raw, []byte(content[:min(len(content), 200)])) && tc.size > 0 {
					t.Error("the spilled content is still in the event line")
				}
				if !bytes.Contains(raw, []byte(written.Blob)) {
					t.Error("the event line does not name the blob it spilled to")
				}
			default:
				if !bytes.Contains(raw, []byte(`"size":`+fmt.Sprint(tc.size))) {
					t.Errorf("the line does not record the size:\n%s", raw)
				}
			}

			// And the value comes back byte for byte.
			got, errs := collectAll(t, j)
			if len(errs) != 0 {
				t.Fatalf("Read: %v", errs)
			}
			if len(got) != 1 {
				t.Fatalf("read %d events, want 1", len(got))
			}
			back := output(t, got[0])
			if back.Inline != content {
				t.Errorf("content changed across the spill: read %d bytes, wrote %d", len(back.Inline), len(content))
			}
			if back.Spilled() != tc.wantSpilled {
				t.Errorf("Spilled() = %v after Read, want %v — where a value is kept is a fact about the record",
					back.Spilled(), tc.wantSpilled)
			}

			// Nothing but finished blobs in the store, and the name is the
			// content's own checksum.
			names := blobNames(t, root)
			if !tc.wantSpilled {
				if len(names) != 0 {
					t.Errorf("a value at or under the threshold spilled anyway: %v", names)
				}
				return
			}
			if len(names) != 1 {
				t.Fatalf("blob store holds %v, want exactly one blob", names)
			}
			sum := sha256.Sum256(readBlob(t, root, names[0]))
			if got := hex.EncodeToString(sum[:]); got != names[0] {
				t.Errorf("blob %s hashes to %s — the name is not the content address", names[0], got)
			}
		})
	}
}

// TestThresholdIsTunable: 64 KiB is a default, not a constant. The same value
// spills or does not depending on the journal it is appended to.
func TestThresholdIsTunable(t *testing.T) {
	content := strings.Repeat("x", 100)

	small := t.TempDir()
	js := open(t, small, journal.WithBlobThreshold(16))
	ev, err := js.Append(context.Background(), 1, journal.ToolResult{
		CallID: "c", Tool: "run_shell", Output: journal.InlineText(content)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !output(t, ev).Spilled() {
		t.Error("100 bytes did not spill at a 16-byte threshold")
	}

	big := t.TempDir()
	jb := open(t, big) // the default
	ev, err = jb.Append(context.Background(), 1, journal.ToolResult{
		CallID: "c", Tool: "run_shell", Output: journal.InlineText(content)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if output(t, ev).Spilled() {
		t.Errorf("100 bytes spilled at the %d-byte default", journal.DefaultBlobThreshold)
	}
	if len(blobNames(t, big)) != 0 {
		t.Error("a journal that spilled nothing created blobs anyway")
	}
}

// TestBlobsAreContentAddressed: the same content written twice is one file, and
// different content is two. Deduplication is not an optimisation here — it is
// what "content-addressed" means, and a store that wrote a second copy under a
// second name would not be one.
func TestBlobsAreContentAddressed(t *testing.T) {
	root := t.TempDir()
	j := open(t, root, journal.WithBlobThreshold(8))

	same := strings.Repeat("the same 200 KiB of test output, twice\n", 5000)
	other := same + "and one line more\n"

	refs := make([]string, 0, 3)
	for _, content := range []string{same, same, other} {
		ev, err := j.Append(context.Background(), 1, journal.ToolResult{
			CallID: "c", Tool: "run_shell", Output: journal.InlineText(content)})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		refs = append(refs, output(t, ev).Blob)
	}

	if refs[0] != refs[1] {
		t.Errorf("identical content got two refs: %s and %s", refs[0], refs[1])
	}
	if refs[0] == refs[2] {
		t.Errorf("different content got the same ref: %s", refs[0])
	}
	if names := blobNames(t, root); len(names) != 2 {
		t.Errorf("blob store holds %d files, want 2 — identical content written twice is one blob", len(names))
	}

	// Both events still read back as themselves.
	got, errs := collectAll(t, j)
	if len(errs) != 0 {
		t.Fatalf("Read: %v", errs)
	}
	for i, want := range []string{same, same, other} {
		if back := output(t, got[i]).Inline; back != want {
			t.Errorf("event %d: read %d bytes, wrote %d", i+1, len(back), len(want))
		}
	}
}

// TestSecretNeverReachesABlob is the guard this card exists to hold.
//
// No payload *field* can hold a credential, and Append scrubs the encoded line
// — but a model that runs `env` in a shell puts the key inside 200 KiB of tool
// output, and 200 KiB does not go in the line. If the spill ran before the
// scrub, or hashed before it, OPENROUTER_API_KEY would sit in
// .kopicode/blobs/<sha256> under a name derived from it, having never touched
// events.jsonl. CLAUDE.md is explicit: not in the journal, not in a blob.
func TestSecretNeverReachesABlob(t *testing.T) {
	// Low-entropy on purpose: a fixture, not a key, and it should not look like
	// one to a secret scanner either.
	const fakeKey = "not-a-real-key-not-a-real-key"
	t.Setenv("OPENROUTER_API_KEY", fakeKey)

	root := t.TempDir()
	// No WithSecretEnv: this is the default path, the one production takes.
	j, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	env := "PATH=/usr/bin\nOPENROUTER_API_KEY=" + fakeKey + "\nHOME=/home/jian\n"
	content := strings.Repeat("VAR=filler value that makes this an oversized dump\n", 4000) + env
	if len(content) <= journal.DefaultBlobThreshold {
		t.Fatalf("the fixture is %d bytes, under the %d-byte threshold — it would not spill and the test would prove nothing",
			len(content), journal.DefaultBlobThreshold)
	}

	ev, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call_1", Tool: "run_shell", Output: journal.InlineText(content)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	ref := output(t, ev).Blob
	if ref == "" {
		t.Fatal("the oversized output did not spill — this test checks the blob path")
	}

	blobPath := filepath.Join(journal.BlobDir(root), ref)
	stored := readBlob(t, root, ref)
	if bytes.Contains(stored, []byte(fakeKey)) {
		t.Fatalf("the key is on disk in %s", blobPath)
	}
	if bytes.Contains(readRaw(t, root), []byte(fakeKey)) {
		t.Fatalf("the key is on disk in %s", eventsPath(root))
	}
	if !bytes.Contains(stored, []byte("[redacted:OPENROUTER_API_KEY]")) {
		t.Error("the blob does not say a value was removed")
	}
	// Redaction removes the secret, not the diagnostic context around it — the
	// output is spilled precisely because a reviewer will read it.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/jian", "VAR=filler value"} {
		if !bytes.Contains(stored, []byte(want)) {
			t.Errorf("redaction removed more than the secret: %q is gone", want)
		}
	}

	// The ordering guard. Hashing before redacting would name bytes that were
	// never written, and every read of this blob would then report corruption
	// against a file that is exactly what it should be.
	sum := sha256.Sum256(stored)
	if got := hex.EncodeToString(sum[:]); got != ref {
		t.Errorf("blob is named %s but hashes to %s — the digest was taken before redaction", ref, got)
	}
	got, errs := collectAll(t, j)
	if len(errs) != 0 {
		t.Fatalf("reading back the redacted blob: %v", errs)
	}
	back := output(t, got[0]).Inline
	if strings.Contains(back, fakeKey) {
		t.Error("Read handed the key back to the caller")
	}
	if !strings.Contains(back, "[redacted:OPENROUTER_API_KEY]") {
		t.Error("the rehydrated value does not say a value was removed")
	}
}

// TestMissingBlobIsReportedAndReadContinues.
//
// A blob that is gone is a sibling of the truncated tail, and gets the opposite
// answer for a stated reason: a cut line makes everything after it unreadable,
// while a lost blob damages one field of one event. So the event is yielded
// *with* its error — the ref and the size are still on the line, so the caller
// can see what is gone — and the events after it are still delivered.
func TestMissingBlobIsReportedAndReadContinues(t *testing.T) {
	root := t.TempDir()
	// High enough that only the middle event spills, so the surviving events on
	// either side are genuinely inline ones the reader still owes the caller.
	j := open(t, root, journal.WithBlobThreshold(64))

	big := strings.Repeat("output that justified the fix\n", 100)
	for i, content := range []string{"first, inline", big, "third, inline"} {
		if _, err := j.Append(context.Background(), i+1, journal.ToolResult{
			CallID: fmt.Sprintf("call_%d", i+1), Tool: "run_shell",
			Output: journal.InlineText(content)}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	names := blobNames(t, root)
	if len(names) != 1 {
		t.Fatalf("blob store holds %v, want the one spilled event", names)
	}
	if err := os.Remove(filepath.Join(journal.BlobDir(root), names[0])); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	got, errs := collectAll(t, j)
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1: %v", len(errs), errs)
	}
	if !errors.Is(errs[0], journal.ErrBlobMissing) {
		t.Fatalf("errors.Is(err, ErrBlobMissing) = false: %v", errs[0])
	}
	var re *journal.ReadError
	if !errors.As(errs[0], &re) {
		t.Fatalf("errors.As(*ReadError) = false: %v", errs[0])
	}
	if re.Line != 2 {
		t.Errorf("ReadError.Line = %d, want 2 — the error must name the event", re.Line)
	}
	if !strings.Contains(errs[0].Error(), names[0]) {
		t.Errorf("the error does not name the blob: %v", errs[0])
	}

	// Three events, not one: iteration continued past the damage.
	if len(got) != 3 {
		t.Fatalf("read %d events, want 3 — one bad blob must not cost the rest of the record", len(got))
	}
	if in := output(t, got[0]).Inline; in != "first, inline" {
		t.Errorf("event 1 = %q", in)
	}
	if in := output(t, got[2]).Inline; in != "third, inline" {
		t.Errorf("event 3 = %q", in)
	}
	// The damaged one keeps its ref and its size, and does not read as empty
	// output from a tool that produced plenty.
	damaged := output(t, got[1])
	if damaged.Blob != names[0] || damaged.Size != int64(len(big)) {
		t.Errorf("the damaged event lost its reference: %+v", damaged)
	}
}

// TestCorruptBlobIsReported: the name is a checksum, so content that does not
// match it is detectable rather than merely suspected. Returning it anyway
// would put bytes in front of a reviewer that no tool ever produced.
func TestCorruptBlobIsReported(t *testing.T) {
	root := t.TempDir()
	j := open(t, root, journal.WithBlobThreshold(8))

	big := strings.Repeat("ok  github.com/leejianrong/kopicode/internal/parse\n", 100)
	if _, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call_1", Tool: "run_shell", Output: journal.InlineText(big)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	names := blobNames(t, root)
	if len(names) != 1 {
		t.Fatalf("blob store holds %v, want one blob", names)
	}
	path := filepath.Join(journal.BlobDir(root), names[0])
	if err := os.WriteFile(path, []byte("FAIL  github.com/leejianrong/kopicode/internal/parse\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, errs := collectAll(t, j)
	if len(errs) != 1 || !errors.Is(errs[0], journal.ErrBlobCorrupt) {
		t.Fatalf("errors.Is(err, ErrBlobCorrupt) = false: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("read %d events, want 1", len(got))
	}
	if in := output(t, got[0]).Inline; in != "" {
		t.Errorf("the substituted content was handed to the caller: %q", in)
	}
}

// TestBlobRefIsRefusedUnlessItIsADigest: a reference is read off a file, and
// joining an arbitrary string to the blob directory is how a journal line
// becomes a file read. The only shape this package writes is 64 hex characters,
// so it is the only shape it follows.
func TestBlobRefIsRefusedUnlessItIsADigest(t *testing.T) {
	root := t.TempDir()
	j := open(t, root, journal.WithBlobThreshold(8))

	ev, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "call_1", Tool: "run_shell",
		Output: journal.InlineText("long enough to spill at eight bytes")})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	ref := output(t, ev).Blob

	// Rewrite the ref by hand, which is what a corrupted or hostile record
	// looks like from the reader's side.
	raw := readRaw(t, root)
	rewritten := bytes.Replace(raw, []byte(ref), []byte("../../../etc/passwd"), 1)
	if bytes.Equal(raw, rewritten) {
		t.Fatal("the ref was not found in the line — the fixture does not test what it says")
	}
	if err := os.WriteFile(eventsPath(root), rewritten, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, errs := collectAll(t, j)
	if len(errs) != 1 || !errors.Is(errs[0], journal.ErrBlobRefInvalid) {
		t.Fatalf("errors.Is(err, ErrBlobRefInvalid) = false: %v", errs)
	}
}

// TestConcurrentAppendWithSpill: the loop is concurrent and a spilled event is
// two file operations instead of one. Run under -race, with content unique per
// writer so a crossed blob reference shows up as wrong content rather than as a
// count that still adds up.
func TestConcurrentAppendWithSpill(t *testing.T) {
	root := t.TempDir()
	j := open(t, root, journal.WithBlobThreshold(64))

	const writers, each = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				body := strings.Repeat(fmt.Sprintf("w%d-%d ", w, i), 40)
				if _, err := j.Append(context.Background(), w, journal.ToolResult{
					CallID: fmt.Sprintf("call_%d_%d", w, i), Tool: "run_shell",
					Output: journal.InlineText(body)}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}

	got, readErrs := collectAll(t, j)
	if len(readErrs) != 0 {
		t.Fatalf("Read: %v", readErrs)
	}
	if len(got) != writers*each {
		t.Fatalf("read %d events, want %d", len(got), writers*each)
	}
	for _, ev := range got {
		result, ok := ev.Payload.(journal.ToolResult)
		if !ok {
			t.Fatalf("payload is %T", ev.Payload)
		}
		var w, i int
		if _, err := fmt.Sscanf(result.CallID, "call_%d_%d", &w, &i); err != nil {
			t.Fatalf("call id %q: %v", result.CallID, err)
		}
		want := strings.Repeat(fmt.Sprintf("w%d-%d ", w, i), 40)
		if result.Output.Inline != want {
			t.Fatalf("%s got the wrong content back: %.40q", result.CallID, result.Output.Inline)
		}
	}
}

// TestBlobsArePrivate: a blob holds whatever the model read, which is as
// private as the events file it was lifted out of.
func TestBlobsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}
	root := t.TempDir()
	j := open(t, root, journal.WithBlobThreshold(8))
	if _, err := j.Append(context.Background(), 1, journal.ToolResult{
		CallID: "c", Tool: "run_shell", Output: journal.InlineText("spill me, I am long enough")}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	names := blobNames(t, root)
	if len(names) != 1 {
		t.Fatalf("blob store holds %v, want one blob", names)
	}
	info, err := os.Stat(filepath.Join(journal.BlobDir(root), names[0]))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("blob mode is %v, want 0600", perm)
	}
}

// TestEveryTextIsReachableByPlainStructTraversal is the guard under the spill's
// reflection.
//
// mapTexts walks struct fields only: a slice header and a pointer both alias
// memory the caller still owns, so following one would edit the caller's payload
// rather than the copy. That is safe exactly as long as no payload puts a Text
// behind an indirection — and the failure if one ever does is silent, because a
// field that is never visited is a field that is never spilled and a 4 MB line
// that nobody notices until it is in a recorded fixture.
func TestEveryTextIsReachableByPlainStructTraversal(t *testing.T) {
	textType := reflect.TypeOf(journal.Text{})

	var check func(typ reflect.Type, path string, indirect bool, seen map[reflect.Type]bool)
	check = func(typ reflect.Type, path string, indirect bool, seen map[reflect.Type]bool) {
		if typ == textType {
			if indirect {
				t.Errorf("%s is a Text behind a pointer, slice or map — the spill walks struct fields "+
					"only, so this field would never be visited and never spill", path)
			}
			return
		}
		if seen[typ] {
			return
		}
		seen[typ] = true
		defer delete(seen, typ)

		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			if typ == reflect.TypeOf([]byte(nil)) {
				return
			}
			check(typ.Elem(), path+"[]", true, seen)
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				if !f.IsExported() {
					continue
				}
				check(f.Type, path+"."+f.Name, indirect, seen)
			}
		default:
		}
	}

	for typ, payload := range fixtures() {
		check(reflect.TypeOf(payload), string(typ), false, map[reflect.Type]bool{})
	}
}
