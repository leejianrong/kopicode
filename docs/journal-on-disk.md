# Reading a kopicode journal from disk

For a program that watches or digests a session without going through `serve` (a REPL running in tmux, a finished `run --print`).
The journal is the one record of a session; everything kopicode prints is derived from it
([AGENTS.md](../AGENTS.md)). The Go types are in `internal/journal`; this page is the shape on disk.

## Where it is

```
<root>/.kopicode/sessions/<session id>/events.jsonl     one session, appended and fsynced a line at a time
<root>/.kopicode/blobs/<sha256>                         spilled values, shared by every session in that root
```

`<root>` is the directory the session was started in. A session id is `<UTC timestamp>-<8 hex>` unless the caller named it with `--session-id`
(REPL and `run --print`), in which case it is that id. `kopicode sessions` lists them.

## A line

One JSON object per line: `schema_version`, `session_id`, `seq` (1, 2, 3, ... in order), `turn`, `type`, `ts` and a `payload` whose shape depends on `type`.
A reader keeps an event type it does not know and ignores a field it does not know: events are a compatibility surface. A last line with no
newline is a write in flight; do not parse it.

## A value that was too big for the line

A string field over 64 KiB is not clipped. The line holds a reference in place of the text:

```json
{"output": {"blob": "9f2c...e1", "size": 183422, "head": "first 2 KiB of the text..."}}
```

where small values are `{"inline": "...", "size": 12}`. The bytes of a spilled value are the file `<root>/.kopicode/blobs/<blob>`, where `<blob>` is the
64 lowercase hex characters in the reference. The name is the sha-256 of the stored bytes, so a reader can verify it. Secrets are scrubbed
before a value is stored, so the blob is what the line would have held. Blobs are written once and never changed or deleted by kopicode.
Never join a `blob` string to a path without checking it is 64 hex characters.

**`head` is a head, not the value.** Beside a `blob`, `head` is the first 2,048 bytes of the stored text, backed up to the start of a character so it never ends
inside one. It is there so a reader can show something without opening the file; its presence is the mark that what it shows is shortened, and `size` is the length
of the whole. A value that is not spilled has no `head` and its `inline` is complete. A value spilled by a kopicode older than this field has a `blob` and no
`head`: that means "not recorded", not "empty".

## Is a question open?

A `PermissionRequested` event is written when kopicode raises a consent request, before it asks anyone, and the matching `PermissionDecided`
(same `request_id`) when the answer arrives, or when it is closed as a denial because no answer could be had. So a `PermissionRequested` with no
`PermissionDecided` after it, in a session still running, means a question is open right now, and the model is waiting on it. The request's `kind`,
`tool` and `detail` say what is asked. A session grant (`allow_session`) that already covers a later request writes the pair together, with nothing in
between. Until KOP-228 the request was written together with the answer, so a reader of an older journal never sees an open one.
`ask` (the model's question to a person) is a different pair: `AskRequested` and `AskAnswered`.

## What the model was told that the tree does not show

`LeftoverNoted` (only with `--leftover-note`, ADR-0039) records the names the model's own edits removed that the engine then found still in the tree, with
`file:line` for each, or a `skipped` reason when the search could not run (which is not "clean"). It is written before the note is shown, at most once per name per prompt.
