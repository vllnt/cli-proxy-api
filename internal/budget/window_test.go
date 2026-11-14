package budget

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func utc(value string) time.Time {
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return ts.UTC()
}

func TestWindowBounds(t *testing.T) {
	paris := mustLoc(t, "Europe/Paris")
	newYork := mustLoc(t, "America/New_York")
	tests := []struct {
		name      string
		window    string
		loc       *time.Location
		now       string
		wantStart string
		wantEnd   string
	}{
		{"day utc", "day", time.UTC, "2026-11-14T15:04:05Z", "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"day utc first instant", "day", time.UTC, "2026-11-14T00:00:00Z", "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"day utc last instant", "day", time.UTC, "2026-11-14T23:59:59Z", "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"},
		{"week saturday", "week", time.UTC, "2026-11-14T12:00:00Z", "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z"},
		{"week sunday belongs to the week that ends it", "week", time.UTC, "2026-11-15T23:59:59Z", "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z"},
		{"week monday starts a new week", "week", time.UTC, "2026-11-16T00:00:00Z", "2026-11-16T00:00:00Z", "2026-11-23T00:00:00Z"},
		{"month mid", "month", time.UTC, "2026-11-14T12:00:00Z", "2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z"},
		{"month end", "month", time.UTC, "2026-11-30T23:59:59Z", "2026-11-01T00:00:00Z", "2026-12-01T00:00:00Z"},
		{"month rolls the year", "month", time.UTC, "2026-12-31T12:00:00Z", "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"month leap february", "month", time.UTC, "2028-02-29T10:00:00Z", "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
		{"week crosses month and year", "week", time.UTC, "2027-01-01T10:00:00Z", "2026-12-28T00:00:00Z", "2027-01-04T00:00:00Z"},
		{"paris day on spring-forward is 23h", "day", paris, "2026-03-29T12:00:00Z", "2026-03-28T23:00:00Z", "2026-03-29T22:00:00Z"},
		{"paris day on fall-back is 25h", "day", paris, "2026-10-25T12:00:00Z", "2026-10-24T22:00:00Z", "2026-10-25T23:00:00Z"},
		{"paris week across spring-forward", "week", paris, "2026-03-29T12:00:00Z", "2026-03-22T23:00:00Z", "2026-03-29T22:00:00Z"},
		{"paris month at the boundary", "month", paris, "2026-10-31T23:30:00Z", "2026-10-31T23:00:00Z", "2026-11-30T23:00:00Z"},
		{"new york day uses local midnight", "day", newYork, "2026-11-14T03:00:00Z", "2026-11-13T05:00:00Z", "2026-11-14T05:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, ok := WindowBounds(tt.window, tt.loc, utc(tt.now))
			if !ok {
				t.Fatal("ok = false")
			}
			if !start.Equal(utc(tt.wantStart)) || !end.Equal(utc(tt.wantEnd)) {
				t.Fatalf("bounds = [%s, %s), want [%s, %s)", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), tt.wantStart, tt.wantEnd)
			}
			if now := utc(tt.now); now.Before(start) || !now.Before(end) {
				t.Fatalf("now %s is outside [%s, %s)", tt.now, start, end)
			}
		})
	}
}

func TestWindowBoundsRejectsUnknownWindow(t *testing.T) {
	for _, window := range []string{"", "hour", "Day"} {
		if _, _, ok := WindowBounds(window, time.UTC, utc("2026-11-14T12:00:00Z")); ok {
			t.Fatalf("window %q must not be accepted", window)
		}
	}
	if _, _, ok := WindowBounds("day", nil, utc("2026-11-14T12:00:00Z")); !ok {
		t.Fatal("nil location must fall back to UTC")
	}
}
