// Package pick holds the rule that chooses which Claude account serves a request.
package pick

import (
	"strings"
	"time"
)

// Provider is the only provider the plugin schedules.
const Provider = "claude"

// IsClaude reports whether provider names the Claude provider.
func IsClaude(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), Provider)
}

// Candidate is one account offered for a pick, with what the plugin knows about it.
type Candidate struct {
	ID       string
	Provider string
	Priority int
	// Reset7d is the last weekly reset time seen, or zero for none.
	Reset7d time.Time
	// Due means the account should be tried once before the order applies.
	Due bool
}

// Rule names the reason a candidate was chosen.
type Rule string

const (
	// RuleProbe: the account was due a try (unobserved, retested, or its reset passed).
	RuleProbe Rule = "probe"
	// RuleSoonestReset: the account's weekly quota resets first.
	RuleSoonestReset Rule = "soonest-reset"
)

// Choice is the chosen account and why.
type Choice struct {
	ID      string
	Rule    Rule
	Reset7d time.Time
}

// Choose picks among the Claude candidates. A due candidate goes first; among
// several, the highest priority, then the lowest ID. Otherwise the earliest weekly
// reset wins, ties going to the higher priority, then the lower ID, and a candidate
// with no known reset sorts after every one that has one. It reports false when
// there is no Claude candidate.
func Choose(candidates []Candidate) (Choice, bool) {
	var best *Candidate
	for i := range candidates {
		c := &candidates[i]
		if !IsClaude(c.Provider) {
			continue
		}
		if best == nil || better(c, best) {
			best = c
		}
	}
	if best == nil {
		return Choice{}, false
	}
	rule := RuleSoonestReset
	if best.Due {
		rule = RuleProbe
	}
	return Choice{ID: best.ID, Rule: rule, Reset7d: best.Reset7d}, true
}

// better reports whether a should be chosen over b.
func better(a, b *Candidate) bool {
	if a.Due != b.Due {
		return a.Due
	}
	if !a.Due {
		aKnown, bKnown := !a.Reset7d.IsZero(), !b.Reset7d.IsZero()
		if aKnown != bKnown {
			return aKnown
		}
		if !a.Reset7d.Equal(b.Reset7d) {
			return a.Reset7d.Before(b.Reset7d)
		}
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}
