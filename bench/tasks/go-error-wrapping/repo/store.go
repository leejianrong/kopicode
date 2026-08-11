// Package store is the in-memory key/value store the scheduler keeps its job
// state in.
package store

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned when a key is absent. Callers match it with
// errors.Is rather than comparing strings.
var ErrNotFound = errors.New("store: key not found")

// ErrEmptyKey is returned when a key is the empty string.
var ErrEmptyKey = errors.New("store: empty key")

// Store maps keys to values.
type Store struct {
	data map[string]string
}

// New returns an empty store.
func New() *Store {
	return &Store{data: make(map[string]string)}
}

// Put stores value under key.
func (s *Store) Put(key, value string) error {
	if key == "" {
		return fmt.Errorf("put: %w", ErrEmptyKey)
	}
	s.data[key] = value
	return nil
}

// Get returns the value stored under key.
func (s *Store) Get(key string) (string, error) {
	value, ok := s.data[key]
	if !ok {
		return "", fmt.Errorf("get %q: %v", key, ErrNotFound)
	}
	return value, nil
}

// Delete removes key from the store.
func (s *Store) Delete(key string) error {
	if _, ok := s.data[key]; !ok {
		return fmt.Errorf("delete %q: %v", key, ErrNotFound)
	}
	delete(s.data, key)
	return nil
}

// Len returns the number of keys held.
func (s *Store) Len() int {
	return len(s.data)
}
