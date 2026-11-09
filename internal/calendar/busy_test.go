package calendar

import (
	"reflect"
	"testing"
	"time"
)

func TestMergeBusy(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 10, h, m, 0, 0, time.UTC) }
	b := func(h1, m1, h2, m2 int) BusyBlock { return BusyBlock{at(h1, m1), at(h2, m2)} }
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   []BusyBlock
		want []BusyBlock
	}{
		{"nil", nil, nil},
		{"one", []BusyBlock{b(9, 0, 10, 0)}, []BusyBlock{b(9, 0, 10, 0)}},
		{"disjoint unsorted", []BusyBlock{b(11, 0, 12, 0), b(9, 0, 10, 0)}, []BusyBlock{b(9, 0, 10, 0), b(11, 0, 12, 0)}},
		{"overlapping", []BusyBlock{b(9, 0, 10, 30), b(10, 0, 11, 0)}, []BusyBlock{b(9, 0, 11, 0)}},
		{"touching", []BusyBlock{b(9, 0, 10, 0), b(10, 0, 11, 0)}, []BusyBlock{b(9, 0, 11, 0)}},
		{"contained", []BusyBlock{b(9, 0, 12, 0), b(10, 0, 11, 0)}, []BusyBlock{b(9, 0, 12, 0)}},
		{"chain", []BusyBlock{b(13, 0, 14, 0), b(9, 0, 10, 0), b(9, 30, 13, 0)}, []BusyBlock{b(9, 0, 14, 0)}},
		{"empty and inverted dropped", []BusyBlock{b(9, 0, 9, 0), b(11, 0, 10, 0), b(12, 0, 13, 0)}, []BusyBlock{b(12, 0, 13, 0)}},
		{"duplicate", []BusyBlock{b(9, 0, 10, 0), b(9, 0, 10, 0)}, []BusyBlock{b(9, 0, 10, 0)}},
		{"zones normalised to UTC", []BusyBlock{{at(17, 0).In(la), at(18, 0).In(la)}}, []BusyBlock{b(17, 0, 18, 0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in []BusyBlock
			if tc.in != nil {
				in = append([]BusyBlock(nil), tc.in...)
			}
			got := MergeBusy(in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MergeBusy(%v) = %v, want %v", tc.in, got, tc.want)
			}
			if tc.in != nil && !reflect.DeepEqual(in, tc.in) {
				t.Fatalf("MergeBusy changed its input: %v", in)
			}
		})
	}
}
