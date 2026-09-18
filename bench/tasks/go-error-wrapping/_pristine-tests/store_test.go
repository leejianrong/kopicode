package store

import (
	"errors"
	"strings"
	"testing"
)

func TestGetMissingIsErrNotFound(t *testing.T) {
	s := New()
	_, err := s.Get("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get error %v does not match ErrNotFound with errors.Is", err)
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("Get error %v does not name the key", err)
	}
}

func TestDeleteMissingIsErrNotFound(t *testing.T) {
	s := New()
	err := s.Delete("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete error %v does not match ErrNotFound with errors.Is", err)
	}
}

func TestPutEmptyKey(t *testing.T) {
	s := New()
	if err := s.Put("", "v"); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("Put error %v does not match ErrEmptyKey", err)
	}
}

func TestRoundTrip(t *testing.T) {
	s := New()
	if err := s.Put("k", "v"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get("k")
	if err != nil || got != "v" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}
