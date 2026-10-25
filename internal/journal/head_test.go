package journal_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/leejianrong/kopicode/internal/journal"
)

// KOP-229: a spilled value carries a head, marked by sitting beside the blob.

func TestASpilledValueCarriesAHeadAndASmallOneDoesNot(t *testing.T) {
	root := t.TempDir()
	j := open(t, root, journal.WithBlobThreshold(4096))
	big := strings.Repeat("0123456789abcdef\n", 600) // 10,200 bytes
	small := "short output\n"

	ev1, err := j.Append(context.Background(), 1, journal.ToolResult{CallID: "c1", Tool: "run_shell", Output: journal.InlineText(big)})
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := j.Append(context.Background(), 1, journal.ToolResult{CallID: "c2", Tool: "run_shell", Output: journal.InlineText(small)})
	if err != nil {
		t.Fatal(err)
	}

	got := output(t, ev1)
	if !got.Spilled() || len(got.Head) != journal.HeadBytes || got.Head != big[:journal.HeadBytes] {
		t.Errorf("spilled value: spilled=%v head=%d bytes, want %d bytes equal to the start of the value",
			got.Spilled(), len(got.Head), journal.HeadBytes)
	}
	if got.Size != int64(len(big)) {
		t.Errorf("size = %d, want the whole value's %d", got.Size, len(big))
	}
	if !bytes.HasPrefix(readBlob(t, root, got.Blob), []byte(got.Head)) {
		t.Error("the head is not the start of the blob")
	}
	if !bytes.Contains(readRaw(t, root), []byte(`"head":`)) {
		t.Error("the head is not on the events line")
	}
	if s := output(t, ev2); s.Head != "" || s.Spilled() {
		t.Errorf("a small value has a head or a blob: %+v", s)
	}
}

func TestAHeadNeverEndsInsideARune(t *testing.T) {
	for _, pad := range []string{"", "a", "ab", "abc"} {
		root := t.TempDir()
		j := open(t, root, journal.WithBlobThreshold(4096))
		// Two- and three-byte runes, so the 2,048th byte lands inside one for some pads.
		content := pad + strings.Repeat("й世", 3000)
		ev, err := j.Append(context.Background(), 1, journal.ToolResult{CallID: "c", Tool: "run_shell", Output: journal.InlineText(content)})
		if err != nil {
			t.Fatal(err)
		}
		head := output(t, ev).Head
		if !utf8.ValidString(head) || len(head) > journal.HeadBytes || len(head) < journal.HeadBytes-utf8.UTFMax {
			t.Errorf("pad %q: head is %d bytes, valid=%v; want valid UTF-8 within %d bytes of the limit",
				pad, len(head), utf8.ValidString(head), utf8.UTFMax)
		}
		if !strings.HasPrefix(content, head) {
			t.Errorf("pad %q: the head is not a prefix of the value", pad)
		}
	}
}

func TestAHeadIsTakenAfterSecretsAreScrubbed(t *testing.T) {
	const fakeKey = "not-a-real-key-not-a-real-key"
	t.Setenv("OPENROUTER_API_KEY", fakeKey)
	root := t.TempDir()
	j, err := journal.Open(root, testSessionID, journal.WithClock(newClock().now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })

	// The key is in the first 2 KiB, where the head would carry it.
	content := "OPENROUTER_API_KEY=" + fakeKey + "\n" + strings.Repeat("filler line of output\n", 5000)
	ev, err := j.Append(context.Background(), 1, journal.ToolResult{CallID: "c", Tool: "run_shell", Output: journal.InlineText(content)})
	if err != nil {
		t.Fatal(err)
	}
	head := output(t, ev).Head
	if strings.Contains(head, fakeKey) || bytes.Contains(readRaw(t, root), []byte(fakeKey)) {
		t.Fatal("the key is in the head or on the events line")
	}
	if !strings.Contains(head, "[redacted:OPENROUTER_API_KEY]") {
		t.Errorf("the head does not show the value was removed: %q", head[:80])
	}
}
