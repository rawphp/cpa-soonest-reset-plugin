package pick

import (
	"testing"
	"time"
)

var (
	monday    = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	wednesday = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	friday    = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
)

func claude(id string, priority int, reset time.Time) Candidate {
	return Candidate{ID: id, Provider: "claude", Priority: priority, Reset7d: reset}
}

func due(c Candidate) Candidate {
	c.Due = true
	return c
}

func mustChoose(t *testing.T, candidates []Candidate) Choice {
	t.Helper()
	choice, ok := Choose(candidates)
	if !ok {
		t.Fatalf("Choose(%v) = unhandled; want a choice", candidates)
	}
	return choice
}

func TestEarliestWeeklyResetWins(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		claude("acct-a@example.test", 5, friday),
		claude("acct-b@example.test", 0, monday),
		claude("acct-c@example.test", 9, wednesday),
	})

	want := Choice{ID: "acct-b@example.test", Rule: RuleSoonestReset, Reset7d: monday}
	if choice != want {
		t.Fatalf("Choose = %+v; want %+v", choice, want)
	}
}

func TestTiedResetGoesToTheHigherPriority(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		claude("acct-a@example.test", 1, monday),
		claude("acct-b@example.test", 3, monday),
		claude("acct-c@example.test", 2, monday),
	})

	if choice.ID != "acct-b@example.test" {
		t.Fatalf("Choose picked %q; want acct-b@example.test", choice.ID)
	}
}

func TestTiedResetAndPriorityGoesToTheLowerID(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		claude("acct-c@example.test", 1, monday),
		claude("acct-a@example.test", 1, monday),
		claude("acct-b@example.test", 1, monday),
	})

	if choice.ID != "acct-a@example.test" {
		t.Fatalf("Choose picked %q; want acct-a@example.test", choice.ID)
	}
}

func TestACandidateWithNoResetSortsLast(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		claude("acct-a@example.test", 9, time.Time{}),
		claude("acct-b@example.test", 0, friday),
	})

	if choice.ID != "acct-b@example.test" {
		t.Fatalf("Choose picked %q; want acct-b@example.test", choice.ID)
	}
}

func TestWithNoResetsKnownPriorityThenIDDecides(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		claude("acct-c@example.test", 2, time.Time{}),
		claude("acct-b@example.test", 2, time.Time{}),
		claude("acct-a@example.test", 1, time.Time{}),
	})

	want := Choice{ID: "acct-b@example.test", Rule: RuleSoonestReset}
	if choice != want {
		t.Fatalf("Choose = %+v; want %+v", choice, want)
	}
}

func TestADueCandidateIsTriedFirst(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		claude("acct-a@example.test", 9, monday),
		due(claude("acct-b@example.test", 0, friday)),
	})

	want := Choice{ID: "acct-b@example.test", Rule: RuleProbe, Reset7d: friday}
	if choice != want {
		t.Fatalf("Choose = %+v; want %+v", choice, want)
	}
}

func TestAmongDueCandidatesPriorityThenIDDecides(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		due(claude("acct-d@example.test", 1, monday)),
		due(claude("acct-c@example.test", 4, time.Time{})),
		due(claude("acct-b@example.test", 4, friday)),
		claude("acct-a@example.test", 9, monday),
	})

	if choice.ID != "acct-b@example.test" || choice.Rule != RuleProbe {
		t.Fatalf("Choose = %+v; want acct-b@example.test by probe", choice)
	}
}

func TestOtherProvidersAreIgnored(t *testing.T) {
	choice := mustChoose(t, []Candidate{
		{ID: "codex-acct@example.test", Provider: "codex", Priority: 9, Reset7d: monday},
		due(Candidate{ID: "gemini-acct@example.test", Provider: "gemini", Priority: 9}),
		claude("acct-b@example.test", 0, friday),
	})

	if choice.ID != "acct-b@example.test" {
		t.Fatalf("Choose picked %q; want acct-b@example.test", choice.ID)
	}
}

func TestProviderMatchIgnoresCase(t *testing.T) {
	choice := mustChoose(t, []Candidate{{ID: "acct-a@example.test", Provider: "Claude", Reset7d: monday}})

	if choice.ID != "acct-a@example.test" {
		t.Fatalf("Choose picked %q; want acct-a@example.test", choice.ID)
	}
}

func TestNoClaudeCandidatesIsUnhandled(t *testing.T) {
	if choice, ok := Choose([]Candidate{{ID: "codex-acct@example.test", Provider: "codex", Reset7d: monday}}); ok {
		t.Fatalf("Choose = %+v; want unhandled", choice)
	}
}

func TestNoCandidatesIsUnhandled(t *testing.T) {
	if choice, ok := Choose(nil); ok {
		t.Fatalf("Choose(nil) = %+v; want unhandled", choice)
	}
	if choice, ok := Choose([]Candidate{}); ok {
		t.Fatalf("Choose(empty) = %+v; want unhandled", choice)
	}
}

func TestChooseDoesNotReorderItsInput(t *testing.T) {
	candidates := []Candidate{
		claude("acct-b@example.test", 0, friday),
		claude("acct-a@example.test", 0, monday),
	}

	mustChoose(t, candidates)

	if candidates[0].ID != "acct-b@example.test" {
		t.Fatal("Choose reordered the caller's slice")
	}
}

func TestIsClaude(t *testing.T) {
	for provider, want := range map[string]bool{
		"claude":   true,
		"CLAUDE":   true,
		" claude ": true,
		"codex":    false,
		"":         false,
		"claude-x": false,
	} {
		if got := IsClaude(provider); got != want {
			t.Errorf("IsClaude(%q) = %v; want %v", provider, got, want)
		}
	}
}
