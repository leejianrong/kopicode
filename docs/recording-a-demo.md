# Recording the README demo video

The demo at the top of the README is a 27 second terminal recording of kopicode fixing a
failing Go test with a live model. It is produced entirely by a script, so anyone can redo it
after the REPL output changes. This page covers how, and the traps that cost the first
recording most of an afternoon.

The quick path, once the tools below are installed:

```bash
make build
export OPENROUTER_API_KEY=...  # from your own secret store; never paste it into a chat or a tape
docs/demo/record.sh           # about 2 minutes and $0.04; writes /tmp/kd/kopicode-demo.mp4
```

Then look at the output (see [Check the result](#check-the-result)) and host it (see
[Putting it in the README](#putting-it-in-the-readme)).

## What is in `docs/demo/`

- `record.sh` builds a throwaway repo, renders the tape, prints the session timeline, then
  trims, captions and strips the audio.
- `demo.tape` is the [VHS](https://github.com/charmbracelet/vhs) script: what gets typed and
  how long it waits.
- `captions.json` holds the on-screen captions and when they show.

The throwaway repo is a ten line Go `Ordinal` function that returns `11st` and `13rd`, with a
test that fails on both. It lives in `/tmp/kd/ordinal`, never inside this repo, so the video
shows no one's code but the demo's.

## Tools

You need `vhs`, `ttyd`, `ffmpeg`, `go` and python 3. VHS shells out to `ttyd` and a headless
Chromium, and neither installs with `vhs`.

```bash
go install github.com/charmbracelet/vhs@v0.10.0
curl -fsSL -o ~/.local/bin/ttyd \
  https://github.com/tsl0922/ttyd/releases/latest/download/ttyd.x86_64 && chmod +x ~/.local/bin/ttyd
```

The recording skill (`~/.claude/skills/video-demo`) supplies `mux_audio.py`, which burns the
captions in and sizes the file for GitHub. Its `scripts/preflight_check.sh` lists anything
missing. Set `VIDEO_DEMO_SKILL` if the skill lives somewhere else.

## Traps, in the order you will meet them

**A stale `GOROOT` breaks every Go command.** `go install` fails with "cannot find GOROOT
directory: /usr/local/go". `record.sh` unsets it, and so does the first hidden line of the
tape. If you run `go` by hand, `unset GOROOT` first (AGENTS.md has the long version).

**`go` must be on the PATH of the shell VHS starts.** VHS runs a fresh bash, not your login
shell. Without `go` there, the REPL still works but prints `[syntax] no checker available` and
verification is skipped, which quietly removes the best part of the demo. The script inherits
your PATH, so check `command -v go` before you start.

**`Output` takes a relative path.** `Output /tmp/kd/take.mp4` is a parse error. The script
runs VHS from `/tmp/kd` for that reason.

**Do not use `Wait+Screen`.** It looks like the right tool for a live model whose latency
varies. In this setup it read a stale screen (the last value it reported was the `$ kopicode`
line, with the REPL's output already on screen), waited out its timeout and aborted with a
bare "recording failed" and no video. The tape uses a fixed `Sleep 19s` instead. The model
loop took 13 to 20 seconds across five runs, so 19 usually holds. If a take comes out with the
final summary missing, re-record rather than lengthening the sleep, or the video drifts past 30
seconds.

**Keep the work directory short.** The REPL prints `[note] record: <session path>` at the top.
In a scratchpad directory that line leaks your home path and wraps onto two lines. `/tmp/kd` is
short enough to stay on one.

**A take is not repeatable.** The model chooses its own tool calls, so the transcript differs
every run. One take showed a real `[repair] attempt 1: unknown_tool`, and another edit
dropped the `strconv` import and had to recover. Both are fine to show, but captions are
written against one particular take, so re-check them every time (below).

**The consent prompt is not in the demo.** The forced `go test` after an edit runs on its own
and is not gated, so there is no `[perm]` moment to film. A beat built around the
`y/N` prompt would need a task that makes the model call `run_shell` itself.

## How timing works

VHS time and wall-clock time agree closely, which is what makes cues possible. The recording
is a few seconds of typing, then the session starts when the tape presses Enter on `kopicode`
(about 4.0 seconds in), then 5.7 seconds later the task is submitted. The journal then tells
you when everything else happened.

`record.sh` prints the journal timeline after rendering, in seconds from `SessionStarted`. Add
the session offset to get video time:

```
  0.00  SessionStarted        -> video 4.0s
  5.71  UserMessage           -> 9.7s
  6.88  ToolCallRepaired      -> 10.9s
 16.81  EditApplied           -> 20.8s
 17.10  VerificationRun       -> 21.1s
 20.19  AssistantMessage      -> 24.2s
```

Move each caption in `captions.json` so it starts a beat after the thing it describes. The
journal is the source of truth for timing, the same way it is for everything else in kopicode;
do not time things by eye from the video.

If you want sound, `mux_audio.py` also takes `--music <mood>` and a `--cues` file of
`{"sfx": "click", "t": 9.7}` entries. The README version has neither, because GitHub plays
embedded video muted by default.

## Check the result

The script prints the duration and size. Then pull one frame inside each caption window and look
at it:

```bash
ffmpeg -y -ss 13.5 -i /tmp/kd/kopicode-demo.mp4 -frames:v 1 /tmp/kd/check.png
```

Check three things: the caption does not cover terminal text, the line it names is on screen at
that moment, and nothing in the frame is a home directory, a key or a hostname.

## Putting it in the README

GitHub only plays a video inline from a `github.com/user-attachments/assets/...` URL, and the
only way to get one is to drag the file into a comment or editor box on github.com. There is no
API or `gh` command for it. Drag `kopicode-demo.mp4` into any comment box on the repository,
copy the URL it inserts, discard the comment, and put the URL on its own line in `README.md`.
A `.mp4` committed to the repo only shows as a download link, so the video itself never goes
in git; this directory holds only what is needed to remake it.

The free-tier upload limit is 10MB. The captions-only take is about 0.8MB.
