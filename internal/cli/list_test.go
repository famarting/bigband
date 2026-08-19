package cli

import (
	"sort"
	"testing"
)

func TestParseListSort(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{in: "name"},
		{in: "next", want: true},
		{in: "next-run", want: true},
		{in: "bogus", wantErr: true},
	} {
		got, err := parseListSort(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseListSort(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("parseListSort(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Timestamps sort chronologically ahead of status words, which fall back to
// name order — regardless of the order the rows arrive in.
func TestLessByNextRun(t *testing.T) {
	type row struct{ name, next string }
	rows := []row{
		{"zeta", "pending"},
		{"beta", "2026-08-19 09:00:00"},
		{"alpha", "disabled"},
		{"gamma", "2026-08-18 23:30:00"},
		{"delta", "2026-08-19 09:00:00"},
	}
	sort.Slice(rows, func(i, j int) bool {
		return lessByNextRun(rows[i].name, rows[i].next, rows[j].name, rows[j].next)
	})

	want := []string{"gamma", "beta", "delta", "alpha", "zeta"}
	for i, name := range want {
		if rows[i].name != name {
			t.Fatalf("position %d = %q, want %q (order: %v)", i, rows[i].name, name, rows)
		}
	}
}
