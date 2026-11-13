// Package quota reads Anthropic's weekly quota reset time from response headers.
package quota

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// WeeklyResetHeader carries the time the account's 7-day quota window resets.
const WeeklyResetHeader = "Anthropic-Ratelimit-Unified-7d-Reset"

// maxUnixSeconds bounds numeric values so float-to-int conversion stays defined.
// It is far past any real reset time (the year 5138).
const maxUnixSeconds = 1e11

// WeeklyReset returns the weekly reset time from headers. The value is a Unix
// timestamp in seconds or an RFC 3339 time. A missing, unparseable or
// not-in-the-future value gives no reset.
func WeeklyReset(headers map[string][]string, now time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(headerValue(headers, WeeklyResetHeader))
	if raw == "" {
		return time.Time{}, false
	}
	reset, ok := parseTime(raw)
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func parseTime(raw string) (time.Time, bool) {
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		if math.IsNaN(seconds) || seconds <= 0 || seconds > maxUnixSeconds {
			return time.Time{}, false
		}
		whole := int64(seconds)
		return time.Unix(whole, int64((seconds-float64(whole))*1e9)).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}
