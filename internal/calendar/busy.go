package calendar

import (
	"slices"
)

// MergeBusy sorts and merges overlapping or touching blocks. Blocks that
// are empty or end before they start are dropped. The result is in UTC and
// the input is left as it was.
func MergeBusy(blocks []BusyBlock) []BusyBlock {
	var sorted []BusyBlock
	for _, b := range blocks {
		if b.End.After(b.Start) {
			sorted = append(sorted, BusyBlock{Start: b.Start.UTC(), End: b.End.UTC()})
		}
	}
	slices.SortFunc(sorted, func(a, b BusyBlock) int { return a.Start.Compare(b.Start) })
	var out []BusyBlock
	for _, b := range sorted {
		if n := len(out); n > 0 && !b.Start.After(out[n-1].End) {
			if b.End.After(out[n-1].End) {
				out[n-1].End = b.End
			}
			continue
		}
		out = append(out, b)
	}
	return out
}
