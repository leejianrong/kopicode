// Package intervals collapses booking ranges into the smallest set of
// non-overlapping ranges that covers the same minutes.
package intervals

import "sort"

// Interval is a half-open range of minutes: Start is included, End is not.
type Interval struct {
	Start int
	End   int
}

// Merge returns the smallest set of intervals covering the same minutes as in.
// The result is sorted by Start. The input is not modified.
func Merge(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}

	sorted := make([]Interval, len(in))
	copy(sorted, in)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start == sorted[j].Start {
			return sorted[i].End < sorted[j].End
		}
		return sorted[i].Start < sorted[j].Start
	})

	out := []Interval{sorted[0]}
	for _, next := range sorted[1:] {
		last := &out[len(out)-1]
		if next.Start < last.End {
			if next.End > last.End {
				last.End = next.End
			}
			continue
		}
		out = append(out, next)
	}
	return out
}

// TotalLength returns the number of minutes covered by in, counting any minute
// covered by more than one interval only once.
func TotalLength(in []Interval) int {
	total := 0
	for _, iv := range Merge(in) {
		total += iv.End - iv.Start
	}
	return total
}

// Contains reports whether minute falls inside any of the intervals.
func Contains(in []Interval, minute int) bool {
	for _, iv := range in {
		if minute >= iv.Start && minute < iv.End {
			return true
		}
	}
	return false
}
