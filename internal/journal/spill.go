package journal

import (
	"errors"
	"reflect"
)

// textType is the field type the spill looks for. Every unbounded field in
// payload.go is a Text, which is what makes one traversal enough.
var textType = reflect.TypeOf(Text{})

// mapTexts returns a copy of p with fn applied to every Text field it holds, at
// any depth.
//
// # Why reflection, and how far it goes
//
// The alternative is a method per payload type. Nineteen of them today, each a
// hand-written list of which fields are Text — and the failure mode of that
// design is silent: a twentieth type, or a new Text field on an existing one,
// spills nothing and nobody finds out until a 4 MB line lands in the record.
// One traversal that cannot miss a field beats nineteen that can, and
// TestEveryTextIsReachableByPlainStructTraversal holds it to that claim.
//
// It walks struct fields only — not pointers, not slices, not maps. A slice
// header and a pointer both alias memory the caller still owns, so following
// one would edit the caller's payload behind its back rather than editing the
// copy. Nothing in payload.go puts a Text behind either, the credential guards
// in payload_test.go already forbid maps outright, and the reachability test
// fails if that ever stops being true.
//
// A payload that is not a struct — nothing in this package, but the interface
// permits it — is returned unchanged rather than mutated in place.
func mapTexts(p Payload, fn func(Text) Text) Payload {
	v := reflect.ValueOf(p)
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return p
	}
	cp := reflect.New(v.Type()).Elem()
	cp.Set(v)
	walkTexts(cp, fn)
	out, ok := cp.Interface().(Payload)
	if !ok {
		// Unreachable: cp is a copy of a value that satisfied Payload.
		return p
	}
	return out
}

// walkTexts applies fn to every Text reachable from the addressable struct
// value v.
func walkTexts(v reflect.Value, fn func(Text) Text) {
	if v.Type() == textType {
		if t, ok := v.Interface().(Text); ok {
			v.Set(reflect.ValueOf(fn(t)))
		}
		return
	}
	if v.Kind() != reflect.Struct {
		return
	}
	for i := range v.NumField() {
		if !v.Type().Field(i).IsExported() {
			continue
		}
		walkTexts(v.Field(i), fn)
	}
}

// spill moves every oversized Text in payload to a blob and replaces it with a
// reference.
//
// # The order of operations, and why it is not negotiable
//
// Redact, then hash, then write, then reference. Each step depends on the one
// before for a different reason:
//
//   - Redaction comes first because a blob is part of the record. Redacting the
//     encoded line — which is what FileJournal did before this card — would
//     scrub the events file while the API key sat in .kopicode/blobs under a
//     name derived from it. A model running `env` in a shell produces exactly
//     that: output far over the threshold with the key inside it. Scrubbing the
//     content before it is ever hashed makes the bad ordering unrepresentable
//     rather than merely avoided.
//   - Hashing comes after redaction because the name is a promise about the
//     bytes on disk. Hash first and the digest names content that was never
//     written, so every later read reports ErrBlobCorrupt against a blob that is
//     exactly what it should be.
//   - The write completes, and is fsynced, before the event line referencing it
//     is written at all. See blobStore.put.
//
// # Size after redaction
//
// Text.Size stays the length of the value the caller handed over, not the length
// of the redacted bytes in the blob. That is the choice KAN-769 made for the
// inline path — the [redacted:NAME] marker in the content explains the
// difference, and rewriting the number would quietly assert that the shorter
// value is all there ever was. The blob is smaller than Size says exactly when
// something was removed from it, and the content says what.
//
// The threshold is compared against the value as handed over, for the same
// reason: whether a field spills is a property of what the tool produced, not of
// how much of it had to be scrubbed.
func (j *FileJournal) spill(payload Payload) (Payload, error) {
	var err error
	out := mapTexts(payload, func(t Text) Text {
		// One failure stops the rest: the event is not going to be written, so
		// there is nothing to be gained by storing the remaining blobs.
		if err != nil || t.Inline == "" || int64(len(t.Inline)) <= j.blobThreshold {
			return t
		}
		size := int64(len(t.Inline))
		content, _ := j.redactor.scrub([]byte(t.Inline))
		ref, perr := j.blobs.put(content)
		if perr != nil {
			err = perr
			return t
		}
		return BlobText(ref, size)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rehydrate fills in the value behind every blob reference in payload.
//
// Spill is transparent: a caller appends an event with a large field and reads
// back the same value. Fetching the blob is the journal's job, and a caller that
// had to do it would eventually forget to.
//
// The rehydrated Text keeps its Blob reference *and* gains its Inline value.
// Dropping the reference would hide that the content is stored out of line —
// which a surface rendering the record, or anything auditing the store, has a
// legitimate use for — and the two together are what the event actually means.
// On disk exactly one of the two is ever set.
//
// A damaged blob does not cost the caller the event. Every Text is attempted,
// the failures are collected, and the payload comes back with the good fields
// filled in; the bad ones keep their reference and an empty Inline, which is
// what the record says and is not the same as "the tool produced nothing".
func rehydrate(blobs *blobStore, payload Payload) (Payload, error) {
	var errs []error
	out := mapTexts(payload, func(t Text) Text {
		if !t.Spilled() || t.Inline != "" {
			return t
		}
		content, err := blobs.get(t.Blob)
		if err != nil {
			errs = append(errs, err)
			return t
		}
		t.Inline = string(content)
		return t
	})
	return out, errors.Join(errs...)
}
