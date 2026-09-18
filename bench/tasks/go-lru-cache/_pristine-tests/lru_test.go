package lru

import (
	"reflect"
	"testing"
)

func TestPutAndGet(t *testing.T) {
	c := New(2)
	c.Put("a", "1")
	if got, ok := c.Get("a"); !ok || got != "1" {
		t.Errorf(`Get("a") = %q, %v; want "1", true`, got, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Error(`Get("missing") reported a hit`)
	}
}

func TestPutOverwrites(t *testing.T) {
	c := New(2)
	c.Put("a", "1")
	c.Put("a", "2")
	if got, _ := c.Get("a"); got != "2" {
		t.Errorf(`Get("a") = %q, want "2"`, got)
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New(2)
	c.Put("a", "1")
	c.Put("b", "2")

	// Reading "a" makes "b" the least recently used entry, so inserting "c"
	// must evict "b" and keep "a".
	if _, ok := c.Get("a"); !ok {
		t.Fatal(`Get("a") missed before eviction`)
	}
	c.Put("c", "3")

	// Checked before the lookups below, which are themselves reads and so
	// change the order.
	if want := []string{"a", "c"}; !reflect.DeepEqual(c.Keys(), want) {
		t.Errorf("Keys = %v, want %v", c.Keys(), want)
	}
	if _, ok := c.Get("a"); !ok {
		t.Error(`"a" was evicted, but it was read more recently than "b"`)
	}
	if _, ok := c.Get("b"); ok {
		t.Error(`"b" survived, but it was the least recently used entry`)
	}
}

func TestCapacityFloor(t *testing.T) {
	c := New(0)
	c.Put("a", "1")
	c.Put("b", "2")
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}
