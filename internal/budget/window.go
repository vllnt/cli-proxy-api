package budget

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// WindowBounds returns the fixed calendar window [start, end) that contains now.
// Windows start at 00:00 in loc: a day, a week beginning on Monday, or a month.
// The bounds come from time.Date, so a window spanning a DST change is 23, 25 or
// otherwise irregular hours long instead of drifting by an hour. ok is false for an
// unknown window name or when the zone yields bounds that do not contain now.
func WindowBounds(window string, loc *time.Location, now time.Time) (start, end time.Time, ok bool) {
	if loc == nil {
		loc = time.UTC
	}
	t := now.In(loc)
	y, m, d := t.Date()
	switch window {
	case config.APIKeyBudgetWindowDay:
		start = time.Date(y, m, d, 0, 0, 0, 0, loc)
		end = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	case config.APIKeyBudgetWindowWeek:
		sinceMonday := (int(t.Weekday()) + 6) % 7
		start = time.Date(y, m, d-sinceMonday, 0, 0, 0, 0, loc)
		end = time.Date(y, m, d-sinceMonday+7, 0, 0, 0, 0, loc)
	case config.APIKeyBudgetWindowMonth:
		start = time.Date(y, m, 1, 0, 0, 0, 0, loc)
		end = time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
	default:
		return time.Time{}, time.Time{}, false
	}
	if now.Before(start) || !now.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}
