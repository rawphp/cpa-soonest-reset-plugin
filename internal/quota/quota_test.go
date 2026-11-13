package quota

import (
	"testing"
	"time"
)

var now = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func TestWeeklyResetParsesUnixSeconds(t *testing.T) {
	headers := map[string][]string{"Anthropic-Ratelimit-Unified-7d-Reset": {"1794960000"}}

	got, ok := WeeklyReset(headers, now)

	want := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("WeeklyReset = %v, %v; want %v, true", got, ok, want)
	}
}

func TestWeeklyResetParsesFractionalUnixSeconds(t *testing.T) {
	headers := map[string][]string{"Anthropic-Ratelimit-Unified-7d-Reset": {"1794960000.5"}}

	got, ok := WeeklyReset(headers, now)

	want := time.Date(2026, 11, 18, 0, 0, 0, 500_000_000, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("WeeklyReset = %v, %v; want %v, true", got, ok, want)
	}
}

func TestWeeklyResetAcceptsFarFutureUnixSeconds(t *testing.T) {
	headers := map[string][]string{"Anthropic-Ratelimit-Unified-7d-Reset": {"99999999999"}}

	if _, ok := WeeklyReset(headers, now); !ok {
		t.Fatal("WeeklyReset with a far-future value: no reset")
	}
}

func TestWeeklyResetParsesRFC3339(t *testing.T) {
	headers := map[string][]string{"Anthropic-Ratelimit-Unified-7d-Reset": {"2026-11-18T10:30:00+10:00"}}

	got, ok := WeeklyReset(headers, now)

	want := time.Date(2026, 11, 18, 0, 30, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("WeeklyReset = %v, %v; want %v, true", got, ok, want)
	}
}

func TestWeeklyResetIgnoresHeaderNameCase(t *testing.T) {
	for _, name := range []string{
		"anthropic-ratelimit-unified-7d-reset",
		"ANTHROPIC-RATELIMIT-UNIFIED-7D-RESET",
		"Anthropic-Ratelimit-Unified-7D-Reset",
	} {
		headers := map[string][]string{name: {"1794960000"}}
		if _, ok := WeeklyReset(headers, now); !ok {
			t.Errorf("WeeklyReset with header %q: no reset", name)
		}
	}
}

func TestWeeklyResetTrimsWhitespace(t *testing.T) {
	headers := map[string][]string{"Anthropic-Ratelimit-Unified-7d-Reset": {" 1794960000 "}}

	if _, ok := WeeklyReset(headers, now); !ok {
		t.Fatal("WeeklyReset with padded value: no reset")
	}
}

func TestWeeklyResetGivesNoResetForBadValues(t *testing.T) {
	cases := map[string]map[string][]string{
		"nil headers":       nil,
		"missing header":    {"Anthropic-Ratelimit-Unified-5h-Reset": {"1794960000"}},
		"no values":         {"Anthropic-Ratelimit-Unified-7d-Reset": {}},
		"empty value":       {"Anthropic-Ratelimit-Unified-7d-Reset": {""}},
		"blank value":       {"Anthropic-Ratelimit-Unified-7d-Reset": {"   "}},
		"garbage":           {"Anthropic-Ratelimit-Unified-7d-Reset": {"next tuesday"}},
		"zero":              {"Anthropic-Ratelimit-Unified-7d-Reset": {"0"}},
		"negative":          {"Anthropic-Ratelimit-Unified-7d-Reset": {"-5"}},
		"past unix":         {"Anthropic-Ratelimit-Unified-7d-Reset": {"1763683200"}},
		"past RFC 3339":     {"Anthropic-Ratelimit-Unified-7d-Reset": {"2026-11-08T00:00:00Z"}},
		"exactly now":       {"Anthropic-Ratelimit-Unified-7d-Reset": {"2026-11-14T09:00:00Z"}},
		"not a number/time": {"Anthropic-Ratelimit-Unified-7d-Reset": {"1794960000abc"}},
		"NaN":               {"Anthropic-Ratelimit-Unified-7d-Reset": {"NaN"}},
		"infinity":          {"Anthropic-Ratelimit-Unified-7d-Reset": {"+Inf"}},
		"beyond int64":      {"Anthropic-Ratelimit-Unified-7d-Reset": {"1e300"}},
	}
	for name, headers := range cases {
		if got, ok := WeeklyReset(headers, now); ok {
			t.Errorf("%s: WeeklyReset = %v, true; want no reset", name, got)
		}
	}
}
