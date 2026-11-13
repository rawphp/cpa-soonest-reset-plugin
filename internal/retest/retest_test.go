package retest

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

type fakeHost struct {
	auths    []Auth
	listErr  error
	resetErr map[string]error // by auth index
	resets   []string         // auth indexes, in call order
}

func (h *fakeHost) ListAuths() ([]Auth, error) {
	return h.auths, h.listErr
}

func (h *fakeHost) ResetCooldown(authIndex string) error {
	h.resets = append(h.resets, authIndex)
	return h.resetErr[authIndex]
}

type fakeMarker struct {
	rateLimited map[string]bool // by auth ID
	asked       []string
}

func (m *fakeMarker) Retest(id string) bool {
	m.asked = append(m.asked, id)
	return m.rateLimited[id]
}

var auths = []Auth{
	{ID: "claude-acct-a@example.test.json", AuthIndex: "1", Provider: "claude"},
	{ID: "codex-acct-c@example.test.json", AuthIndex: "2", Provider: "codex"},
	{ID: "claude-acct-b@example.test.json", AuthIndex: "3", Provider: "Claude"},
	{ID: "gemini-acct-d@example.test.json", AuthIndex: "4", Provider: "gemini"},
}

func TestPassResetsEveryClaudeAccountAndNoOther(t *testing.T) {
	host := &fakeHost{auths: auths}

	result := Pass(host, &fakeMarker{})

	if want := []string{"1", "3"}; !slices.Equal(host.resets, want) {
		t.Fatalf("reset auth indexes %v; want %v", host.resets, want)
	}
	if want := []string{"claude-acct-a@example.test.json", "claude-acct-b@example.test.json"}; !slices.Equal(result.Reset, want) {
		t.Fatalf("Result.Reset = %v; want %v", result.Reset, want)
	}
}

func TestPassMarksOnlyAccountsTheStoreMarks(t *testing.T) {
	host := &fakeHost{auths: auths}
	marker := &fakeMarker{rateLimited: map[string]bool{"claude-acct-b@example.test.json": true}}

	result := Pass(host, marker)

	if want := []string{"claude-acct-a@example.test.json", "claude-acct-b@example.test.json"}; !slices.Equal(marker.asked, want) {
		t.Fatalf("store asked about %v; want %v", marker.asked, want)
	}
	if want := []string{"claude-acct-b@example.test.json"}; !slices.Equal(result.Marked, want) {
		t.Fatalf("Result.Marked = %v; want %v", result.Marked, want)
	}
}

func TestAResetErrorOnOneAccountDoesNotStopTheRest(t *testing.T) {
	boom := errors.New("auth not found for auth_index 1")
	host := &fakeHost{auths: auths, resetErr: map[string]error{"1": boom}}
	marker := &fakeMarker{rateLimited: map[string]bool{
		"claude-acct-a@example.test.json": true,
		"claude-acct-b@example.test.json": true,
	}}

	result := Pass(host, marker)

	if want := []string{"1", "3"}; !slices.Equal(host.resets, want) {
		t.Fatalf("reset auth indexes %v; want %v", host.resets, want)
	}
	if want := []string{"claude-acct-b@example.test.json"}; !slices.Equal(result.Marked, want) {
		t.Fatalf("Result.Marked = %v; want only the account whose reset worked", result.Marked)
	}
	if len(result.Errors) != 1 || !errors.Is(result.Errors[0], boom) {
		t.Fatalf("Result.Errors = %v; want the one reset error", result.Errors)
	}
}

func TestAListErrorEndsThePass(t *testing.T) {
	boom := errors.New("auth directory is unavailable")
	host := &fakeHost{listErr: boom}

	result := Pass(host, &fakeMarker{})

	if len(host.resets) != 0 {
		t.Fatalf("reset %v after a list error", host.resets)
	}
	if len(result.Errors) != 1 || !errors.Is(result.Errors[0], boom) {
		t.Fatalf("Result.Errors = %v; want the list error", result.Errors)
	}
}

func TestScheduleRunsOnlyAfterTheInterval(t *testing.T) {
	s := NewSchedule(start, time.Hour)

	if s.Due(start) || s.Due(start.Add(59*time.Minute)) {
		t.Fatal("due before the interval passed")
	}
	if !s.Due(start.Add(time.Hour)) {
		t.Fatal("not due once the interval passed")
	}
	if s.Due(start.Add(time.Hour + time.Minute)) {
		t.Fatal("due again straight after running")
	}
	if !s.Due(start.Add(2*time.Hour + time.Minute)) {
		t.Fatal("not due a full interval after the last run")
	}
}

func TestScheduleWithIntervalZeroNeverRuns(t *testing.T) {
	s := NewSchedule(start, 0)

	for _, at := range []time.Time{start, start.Add(time.Hour), start.Add(365 * 24 * time.Hour)} {
		if s.Due(at) {
			t.Fatalf("interval 0 was due at %v", at)
		}
	}
}

func TestScheduleWithANegativeIntervalNeverRuns(t *testing.T) {
	s := NewSchedule(start, -time.Minute)

	if s.Due(start.Add(time.Hour)) {
		t.Fatal("negative interval was due")
	}
}

func TestSetIntervalKeepsTheLastRun(t *testing.T) {
	s := NewSchedule(start, time.Hour)

	s.SetInterval(2 * time.Hour)
	if s.Due(start.Add(time.Hour)) {
		t.Fatal("due on the old interval")
	}
	if !s.Due(start.Add(2 * time.Hour)) {
		t.Fatal("not due on the new interval")
	}

	s.SetInterval(0)
	if s.Due(start.Add(10 * time.Hour)) {
		t.Fatal("due after the interval was set to 0")
	}
}

func TestOnlyOneConcurrentCallerGetsTheRun(t *testing.T) {
	s := NewSchedule(start, time.Hour)
	var mu sync.Mutex
	runs := 0
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if s.Due(start.Add(time.Hour)) {
				mu.Lock()
				runs++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if runs != 1 {
		t.Fatalf("%d callers got the run; want 1", runs)
	}
}
