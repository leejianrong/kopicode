package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/leejianrong/kopicode/internal/journal"
)

// SaveResult and [LoadResult] are KAN-943's half of the paired story: [Compare]
// already takes two [Arm] values and scores them, but nothing before this file
// could get a [RunResult] into a file and back out, so a paired A/B needed two
// invocations of the runner to be still in memory at the same time — which two
// invocations of one binary (`kopibench run`, twice) never are. This is what
// `kopibench compare <a> <b>` reads.
//
// # Why not encoding/json's default field names on [RunResult] itself
//
// A capitalised-field-name encoding of [RunResult] round-trips every field
// except one, and the one it does not round-trip is not survivable: on
// [TaskResult.Oracle], [OracleResult.Err] is a plain `error`, and
// encoding/json marshals a non-nil error by reflecting its *concrete* type.
// The concrete types this codebase actually produces —
// `*errors.errorString`, `*fmt.wrapError` — hold their message in an
// unexported field, so json.Marshal emits `{}` and the message is gone
// silently, with no error from Marshal to say so. That is exactly the failure
// this project's redaction and blob-spill code elsewhere refuses to allow
// happen quietly, and it would happen here by default. So [OracleResult.Err]
// is carried across the boundary as a string — Error() going out, [errors.New]
// coming back in — which loses the original error's type but keeps every byte
// of its message, and nothing downstream needs the type back: a loaded
// [TaskResult] is read for its Passed bit and its report fields, never
// type-switched on its oracle's error.
//
// The rest of the shape gets explicit `snake_case` json tags rather than
// riding on encoding/json's default (capitalised Go field names) for a
// different reason: this file is meant to be opened by a person as well as
// read by `compare`, and the journal package already set the convention this
// follows.
type persistedRun struct {
	RunID         string          `json:"run_id"`
	Arm           persistedArm    `json:"arm"`
	CorpusVersion string          `json:"corpus_version"`
	CorpusDigest  string          `json:"corpus_digest"`
	Commit        string          `json:"commit"`
	Tasks         []persistedTask `json:"tasks"`
	Reclamation   Reclamation     `json:"reclamation"`
	Jobs          int             `json:"jobs"`
	Started       time.Time       `json:"started"`
	DurationMS    int64           `json:"duration_ms"`
	OutDir        string          `json:"out_dir"`
	Provider      ProviderKind    `json:"provider"`
}

type persistedArm struct {
	ModelID           string            `json:"model_id"`
	HarnessConfigHash string            `json:"harness_config_hash"`
	HarnessConfigName string            `json:"harness_config_name"`
	ProviderPin       string            `json:"provider_pin"`
	Build             journal.BuildInfo `json:"build"`
}

type persistedTask struct {
	TaskID       string              `json:"task_id"`
	Passed       bool                `json:"passed"`
	SessionID    string              `json:"session_id"`
	JournalDir   string              `json:"journal_dir"`
	Stop         string              `json:"stop"`
	Turns        int                 `json:"turns"`
	Tokens       journal.TokenCounts `json:"tokens"`
	SessionErr   string              `json:"session_err"`
	Panicked     bool                `json:"panicked"`
	Oracle       persistedOracle     `json:"oracle"`
	Bucket       Bucket              `json:"bucket"`
	Worktree     string              `json:"worktree"`
	WorktreeKept bool                `json:"worktree_kept"`
	DurationMS   int64               `json:"duration_ms"`
}

type persistedOracle struct {
	Argv       []string `json:"argv"`
	Passed     bool     `json:"passed"`
	ExitCode   int      `json:"exit_code"`
	Signal     string   `json:"signal"`
	TimedOut   bool     `json:"timed_out"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
	Err        string   `json:"err"`
}

func toPersisted(r *RunResult) persistedRun {
	p := persistedRun{
		RunID: r.RunID,
		Arm: persistedArm{
			ModelID:           r.Arm.ModelID,
			HarnessConfigHash: r.Arm.HarnessConfigHash,
			HarnessConfigName: r.Arm.HarnessConfigName,
			ProviderPin:       r.Arm.ProviderPin,
			Build:             r.Arm.Build,
		},
		CorpusVersion: r.CorpusVersion,
		CorpusDigest:  r.CorpusDigest,
		Commit:        r.Commit,
		Tasks:         make([]persistedTask, 0, len(r.Tasks)),
		Reclamation:   r.Reclamation,
		Jobs:          r.Jobs,
		Started:       r.Started,
		DurationMS:    r.Duration.Milliseconds(),
		OutDir:        r.OutDir,
		Provider:      r.Provider,
	}
	for _, t := range r.Tasks {
		errText := ""
		if t.Oracle.Err != nil {
			errText = t.Oracle.Err.Error()
		}
		p.Tasks = append(p.Tasks, persistedTask{
			TaskID:     t.TaskID,
			Passed:     t.Passed,
			SessionID:  t.SessionID,
			JournalDir: t.JournalDir,
			Stop:       t.Stop,
			Turns:      t.Turns,
			Tokens:     t.Tokens,
			SessionErr: t.SessionErr,
			Panicked:   t.Panicked,
			Oracle: persistedOracle{
				Argv:       t.Oracle.Argv,
				Passed:     t.Oracle.Passed,
				ExitCode:   t.Oracle.ExitCode,
				Signal:     t.Oracle.Signal,
				TimedOut:   t.Oracle.TimedOut,
				DurationMS: t.Oracle.Duration.Milliseconds(),
				Output:     t.Oracle.Output,
				Err:        errText,
			},
			Bucket:       t.Bucket,
			Worktree:     t.Worktree,
			WorktreeKept: t.WorktreeKept,
			DurationMS:   t.Duration.Milliseconds(),
		})
	}
	return p
}

func fromPersisted(p persistedRun) *RunResult {
	r := &RunResult{
		RunID: p.RunID,
		Arm: ArmIdentity{
			ModelID:           p.Arm.ModelID,
			HarnessConfigHash: p.Arm.HarnessConfigHash,
			HarnessConfigName: p.Arm.HarnessConfigName,
			ProviderPin:       p.Arm.ProviderPin,
			Build:             p.Arm.Build,
		},
		CorpusVersion: p.CorpusVersion,
		CorpusDigest:  p.CorpusDigest,
		Commit:        p.Commit,
		Tasks:         make([]TaskResult, 0, len(p.Tasks)),
		Reclamation:   p.Reclamation,
		Jobs:          p.Jobs,
		Started:       p.Started,
		Duration:      time.Duration(p.DurationMS) * time.Millisecond,
		OutDir:        p.OutDir,
		Provider:      p.Provider,
	}
	for _, t := range p.Tasks {
		var oracleErr error
		if t.Oracle.Err != "" {
			oracleErr = errors.New(t.Oracle.Err)
		}
		r.Tasks = append(r.Tasks, TaskResult{
			TaskID:     t.TaskID,
			Passed:     t.Passed,
			SessionID:  t.SessionID,
			JournalDir: t.JournalDir,
			Stop:       t.Stop,
			Turns:      t.Turns,
			Tokens:     t.Tokens,
			SessionErr: t.SessionErr,
			Panicked:   t.Panicked,
			Oracle: OracleResult{
				Argv:     t.Oracle.Argv,
				Passed:   t.Oracle.Passed,
				ExitCode: t.Oracle.ExitCode,
				Signal:   t.Oracle.Signal,
				TimedOut: t.Oracle.TimedOut,
				Duration: time.Duration(t.Oracle.DurationMS) * time.Millisecond,
				Output:   t.Oracle.Output,
				Err:      oracleErr,
			},
			Bucket:       t.Bucket,
			Worktree:     t.Worktree,
			WorktreeKept: t.WorktreeKept,
			Duration:     time.Duration(t.DurationMS) * time.Millisecond,
		})
	}
	return r
}

// SaveResult writes r as indented JSON to w, in the shape [LoadResult] reads
// back.
//
// HTML escaping is switched off for the reason [journal.Marshal]'s own doc
// comment gives: encoding/json's default re-escapes `<`, `>` and `&` into
// `\uXXXX` sequences meant for embedding JSON inside HTML, which this file is
// never destined for, and a worktree or oracle-output path containing one of
// those bytes should read back exactly as it was rather than gain escape
// noise.
func SaveResult(w io.Writer, r *RunResult) error {
	if r == nil {
		return fmt.Errorf("bench: SaveResult: nil result")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(toPersisted(r)); err != nil {
		return fmt.Errorf("bench: encoding result: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("bench: writing result: %w", err)
	}
	return nil
}

// LoadResult reads back a [RunResult] [SaveResult] wrote.
//
// It refuses a file with no run id and no tasks — the empty-Options zero
// value that would otherwise round-trip as a legitimate-looking empty run —
// because a `compare` invocation given that would print a report on a
// comparison that never happened rather than the parse failure it actually
// is.
func LoadResult(r io.Reader) (*RunResult, error) {
	var p persistedRun
	dec := json.NewDecoder(r)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("bench: decoding result: %w", err)
	}
	if p.RunID == "" && len(p.Tasks) == 0 {
		return nil, fmt.Errorf("bench: decoding result: %w", ErrEmptyResult)
	}
	return fromPersisted(p), nil
}
