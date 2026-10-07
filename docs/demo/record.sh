#!/usr/bin/env bash
# Re-record the README demo video. See docs/recording-a-demo.md before running it.
# Needs: OPENROUTER_API_KEY in the environment, vhs + ttyd + ffmpeg + go on PATH, bin/kopicode built.
# Spends real tokens (about $0.04). Writes /tmp/kd/kopicode-demo.mp4 (captions only, no audio).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
here="$root/docs/demo"
# A short, fixed work dir on purpose: kopicode prints the session path on screen, and a
# long path both leaks your home directory and wraps over two lines in the video.
kd=/tmp/kd
skill=${VIDEO_DEMO_SKILL:-$HOME/.claude/skills/video-demo}

[ -n "${OPENROUTER_API_KEY:-}" ] || { echo "OPENROUTER_API_KEY is not set" >&2; exit 1; }
[ -x "$root/bin/kopicode" ] || { echo "run 'make build' first" >&2; exit 1; }
for tool in vhs ttyd ffmpeg go; do
	command -v "$tool" >/dev/null || { echo "missing: $tool" >&2; exit 1; }
done
unset GOROOT # a stale GOROOT breaks every go command, in the repo and inside VHS's shell

rm -rf "$kd" && mkdir -p "$kd/ordinal"
cd "$kd/ordinal"
cat >go.mod <<'EOF'
module example.com/ordinal

go 1.22
EOF
cat >ordinal.go <<'EOF'
package ordinal

import "strconv"

// Ordinal formats n with its English ordinal suffix: 1st, 2nd, 3rd, 4th.
func Ordinal(n int) string {
	s := strconv.Itoa(n)
	switch n % 10 {
	case 1:
		return s + "st"
	case 2:
		return s + "nd"
	case 3:
		return s + "rd"
	}
	return s + "th"
}
EOF
cat >ordinal_test.go <<'EOF'
package ordinal

import "testing"

func TestOrdinal(t *testing.T) {
	cases := map[int]string{
		1: "1st", 2: "2nd", 3: "3rd", 4: "4th",
		11: "11th", 12: "12th", 13: "13th",
		21: "21st", 22: "22nd", 101: "101st", 111: "111th",
	}
	for n, want := range cases {
		if got := Ordinal(n); got != want {
			t.Errorf("Ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}
EOF
git init -q && git add -A && git -c user.name=demo -c user.email=demo@example.com commit -qm init

cd "$kd"
KD_BIN_DIR="$root/bin" vhs "$here/demo.tape"

# The journal is the clock. Seconds are relative to SessionStarted; the session starts when
# the tape presses Enter on "kopicode" (about 4.0s into the video).
echo "--- journal timeline (add the session offset to get video time; check captions.json) ---"
python3 - "$kd/ordinal" <<'EOF'
import datetime as d, glob, json, sys
ev = [json.loads(l) for l in open(glob.glob(sys.argv[1] + "/.kopicode/sessions/*/events.jsonl")[0])]
ts = lambda e: d.datetime.fromisoformat(e["ts"].replace("Z", "+00:00")[:26] + "+00:00")
t0 = ts(ev[0])
keep = {"SessionStarted", "UserMessage", "ToolCallRepaired", "EditApplied", "SyntaxGateRun", "VerificationRun", "AssistantMessage"}
for e in ev:
    if e["type"] in keep:
        print(f"{(ts(e) - t0).total_seconds():6.2f}  {e['type']}")
EOF

# Trim the idle tail, burn captions, then drop the audio track (no music, no effects).
ffmpeg -v error -y -i take.mp4 -t 27.6 -c:v libx264 -crf 14 -pix_fmt yuv420p trimmed.mp4
python3 "$skill/scripts/mux_audio.py" --video trimmed.mp4 --music upbeat-tech --music-volume 0 \
	--captions "$here/captions.json" --out with-audio.mp4
ffmpeg -v error -y -i with-audio.mp4 -c copy -an kopicode-demo.mp4
ffprobe -v error -show_entries format=duration,size -of compact kopicode-demo.mp4
echo "wrote $kd/kopicode-demo.mp4: check a frame inside every caption window before using it"
