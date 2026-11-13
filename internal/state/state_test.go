package state

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	now      = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	nextWeek = now.Add(4 * 24 * time.Hour)
)

// pickFirst chooses the first id it is given and reports what it saw.
func pickFirst(id string, seen *View) func(map[string]View) string {
	return func(views map[string]View) string {
		*seen = views[id]
		return id
	}
}

func TestUnknownAccountIsDue(t *testing.T) {
	s := New()

	if v := s.View("acct-a@example.test", now); !v.Due || !v.Reset7d.IsZero() {
		t.Fatalf("View = %+v; want due with no reset", v)
	}
}

func TestObserveStoresAndClearsTheWeeklyReset(t *testing.T) {
	s := New()

	s.Observe("acct-a@example.test", nextWeek, false)
	if v := s.View("acct-a@example.test", now); v.Due || !v.Reset7d.Equal(nextWeek) {
		t.Fatalf("after observing a reset: View = %+v; want not due, reset %v", v, nextWeek)
	}

	s.Observe("acct-a@example.test", time.Time{}, false)
	if v := s.View("acct-a@example.test", now); v.Due || !v.Reset7d.IsZero() {
		t.Fatalf("after observing no reset: View = %+v; want not due, no reset", v)
	}
}

func TestAPassedWeeklyResetMakesTheAccountDueAgain(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, false)

	if v := s.View("acct-a@example.test", nextWeek.Add(-time.Second)); v.Due {
		t.Fatal("due before the reset passed")
	}
	if v := s.View("acct-a@example.test", nextWeek); !v.Due {
		t.Fatal("not due once the reset time is reached")
	}
	if v := s.View("acct-a@example.test", nextWeek.Add(time.Hour)); !v.Due {
		t.Fatal("not due after the reset passed")
	}
}

func TestPickingADueAccountMarksItPendingUntilItAnswers(t *testing.T) {
	s := New()
	var seen View

	got := s.Pick([]string{"acct-a@example.test"}, now, pickFirst("acct-a@example.test", &seen))
	if got != "acct-a@example.test" || !seen.Due {
		t.Fatalf("Pick = %q, saw %+v; want acct-a@example.test seen as due", got, seen)
	}
	if v := s.View("acct-a@example.test", now); v.Due {
		t.Fatal("still due after being picked as a probe")
	}

	s.Pick([]string{"acct-a@example.test"}, now, pickFirst("acct-a@example.test", &seen))
	if seen.Due {
		t.Fatal("second pick saw the pending account as due")
	}

	s.Observe("acct-a@example.test", nextWeek, false)
	if v := s.View("acct-a@example.test", now); v.Due || !v.Reset7d.Equal(nextWeek) {
		t.Fatalf("after the answer: View = %+v; want not due, reset %v", v, nextWeek)
	}
}

func TestPickingAPassedResetAccountProbesItOnce(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, false)
	later := nextWeek.Add(time.Minute)
	var seen View

	s.Pick([]string{"acct-a@example.test"}, later, pickFirst("acct-a@example.test", &seen))
	if !seen.Due {
		t.Fatal("first pick after the reset did not see the account as due")
	}
	s.Pick([]string{"acct-a@example.test"}, later, pickFirst("acct-a@example.test", &seen))
	if seen.Due {
		t.Fatal("second pick before an answer saw the account as due")
	}
}

func TestPickOnlyOffersTheGivenIDs(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, false)

	s.Pick([]string{"acct-b@example.test"}, now, func(views map[string]View) string {
		if len(views) != 1 {
			t.Fatalf("views = %v; want only acct-b@example.test", views)
		}
		if _, ok := views["acct-b@example.test"]; !ok {
			t.Fatalf("views = %v; missing acct-b@example.test", views)
		}
		return ""
	})
}

func TestPickingNothingOrAnUnknownIDChangesNothing(t *testing.T) {
	s := New()

	if got := s.Pick([]string{"acct-a@example.test"}, now, func(map[string]View) string { return "" }); got != "" {
		t.Fatalf("Pick = %q; want empty", got)
	}
	if got := s.Pick([]string{"acct-a@example.test"}, now, func(map[string]View) string { return "acct-z@example.test" }); got != "" {
		t.Fatalf("Pick = %q; want empty for an id that was not offered", got)
	}
	if v := s.View("acct-a@example.test", now); !v.Due {
		t.Fatal("account stopped being due though it was never picked")
	}
}

func TestPickingAnAccountThatIsNotDueLeavesItAlone(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, false)
	var seen View

	s.Pick([]string{"acct-a@example.test"}, now, pickFirst("acct-a@example.test", &seen))

	// The reset passing must still make it due: a non-probe pick does not mark it pending.
	if v := s.View("acct-a@example.test", nextWeek); !v.Due {
		t.Fatal("a normal pick marked the account pending")
	}
}

func TestRetestMarksAnAccountWhoseLastAnswerWasA429(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, true)

	if !s.Retest("acct-a@example.test") {
		t.Fatal("Retest = false for an account whose last answer was a 429")
	}
	if v := s.View("acct-a@example.test", now); !v.Due {
		t.Fatal("not due after a retest")
	}
	if s.Retest("acct-a@example.test") {
		t.Fatal("second Retest = true; the 429 was already handled")
	}
}

func TestRetestLeavesOtherAccountsAlone(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, false)

	if s.Retest("acct-a@example.test") {
		t.Fatal("Retest = true for an account that answered without a 429")
	}
	if s.Retest("acct-z@example.test") {
		t.Fatal("Retest = true for an unknown account")
	}
	if v := s.View("acct-a@example.test", now); v.Due {
		t.Fatal("a retest made a healthy account due")
	}
}

func TestASuccessAfterA429ClearsTheRetestMark(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, true)
	s.Observe("acct-a@example.test", nextWeek, false)

	if s.Retest("acct-a@example.test") {
		t.Fatal("Retest = true though the last answer was not a 429")
	}
}

func TestProbeFlagSetsOnRetestAndClearsOnPick(t *testing.T) {
	s := New()
	s.Observe("acct-a@example.test", nextWeek, true)
	s.Retest("acct-a@example.test")
	var seen View

	s.Pick([]string{"acct-a@example.test"}, now, pickFirst("acct-a@example.test", &seen))
	if !seen.Due {
		t.Fatal("probe-due account not seen as due")
	}
	s.Pick([]string{"acct-a@example.test"}, now, pickFirst("acct-a@example.test", &seen))
	if seen.Due {
		t.Fatal("probe flag not cleared by the pick")
	}
}

func TestProbeFlagOverridesAPendingProbe(t *testing.T) {
	s := New()
	var seen View
	s.Pick([]string{"acct-a@example.test"}, now, pickFirst("acct-a@example.test", &seen)) // pending, never answers
	s.Observe("acct-a@example.test", nextWeek, true)                                     // a later 429 answers it
	s.Retest("acct-a@example.test")

	if v := s.View("acct-a@example.test", now); !v.Due {
		t.Fatal("probe-due account not due")
	}
}

func TestOnlyOneConcurrentPickProbesAnAccount(t *testing.T) {
	s := New()
	var probes atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			s.Pick([]string{"acct-a@example.test"}, now, func(views map[string]View) string {
				if views["acct-a@example.test"].Due {
					probes.Add(1)
				}
				return "acct-a@example.test"
			})
		})
	}
	wg.Wait()

	if got := probes.Load(); got != 1 {
		t.Fatalf("%d picks saw the account as due; want exactly 1", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := range 32 {
		id := fmt.Sprintf("acct-%d@example.test", i%4)
		wg.Go(func() {
			for j := range 100 {
				switch j % 4 {
				case 0:
					s.Observe(id, nextWeek, j%8 == 0)
				case 1:
					s.Pick([]string{id, "acct-x@example.test"}, now, func(map[string]View) string { return id })
				case 2:
					s.Retest(id)
				default:
					s.View(id, now)
				}
			}
		})
	}
	wg.Wait()
}
