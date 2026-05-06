package schedule_test

import (
	"testing"

	"github.com/famarting/bigband/internal/schedule"
)

func TestParse(t *testing.T) {
	tests := []struct {
		input      string
		wantCron   string
		wantJitter bool
		wantErr    bool
	}{
		// Standard cron passthrough
		{"0 20 * * 1-5", "0 20 * * 1-5", false, false},
		{"*/5 * * * *", "*/5 * * * *", false, false},
		// Robfig descriptors
		{"@daily", "@daily", false, false},
		{"@every 10m", "@every 10m", false, false},
		{"@every 2h", "@every 2h", false, false},
		// Human DSL — no jitter
		{"Weekdays at 20:00", "0 20 * * 1-5", false, false},
		{"weekdays at 20:00", "0 20 * * 1-5", false, false},
		{"Weekends at 9:30", "30 9 * * 0,6", false, false},
		{"Daily at midnight", "0 0 * * *", false, false},
		{"Daily at noon", "0 12 * * *", false, false},
		{"Mondays at 9am", "0 9 * * 1", false, false},
		{"Fridays at 18:00", "0 18 * * 5", false, false},
		{"Mon, Wed, Fri at 14:30", "30 14 * * 1,3,5", false, false},
		{"Every 10 minutes", "@every 10m", false, false},
		{"every 2 hours", "@every 2h", false, false},
		// Human DSL — with jitter
		{"Weekdays at ~20:00", "0 20 * * 1-5", true, false},
		{"Daily at ~8:00", "0 8 * * *", true, false},
		// Invalid
		{"not a schedule", "", false, true},
		{"Weekdays at badtime", "", false, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			cron, jitter, err := schedule.Parse(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cron=%q jitter=%v", cron, jitter)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cron != tc.wantCron {
				t.Errorf("cron: got %q, want %q", cron, tc.wantCron)
			}
			if jitter != tc.wantJitter {
				t.Errorf("jitter: got %v, want %v", jitter, tc.wantJitter)
			}
		})
	}
}
