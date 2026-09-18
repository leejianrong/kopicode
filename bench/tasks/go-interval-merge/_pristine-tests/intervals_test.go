package intervals

import (
	"reflect"
	"testing"
)

func TestMergeOverlapping(t *testing.T) {
	got := Merge([]Interval{{1, 5}, {3, 8}, {20, 25}})
	want := []Interval{{1, 8}, {20, 25}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestMergeAdjacent(t *testing.T) {
	got := Merge([]Interval{{1, 3}, {3, 5}, {5, 9}})
	want := []Interval{{1, 9}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestMergeLeavesGaps(t *testing.T) {
	got := Merge([]Interval{{10, 12}, {1, 3}})
	want := []Interval{{1, 3}, {10, 12}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestMergeDoesNotModifyInput(t *testing.T) {
	in := []Interval{{5, 9}, {1, 3}}
	Merge(in)
	if in[0] != (Interval{5, 9}) {
		t.Errorf("Merge modified its input: %v", in)
	}
}

func TestTotalLength(t *testing.T) {
	if got := TotalLength([]Interval{{1, 3}, {3, 5}}); got != 4 {
		t.Errorf("TotalLength = %d, want 4", got)
	}
	if got := TotalLength(nil); got != 0 {
		t.Errorf("TotalLength(nil) = %d, want 0", got)
	}
}

func TestContains(t *testing.T) {
	in := []Interval{{1, 3}}
	if !Contains(in, 1) {
		t.Error("Contains(1) = false, want true")
	}
	if Contains(in, 3) {
		t.Error("Contains(3) = true, want false: End is exclusive")
	}
}
