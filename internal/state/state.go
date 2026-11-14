// Package state keeps what the plugin knows about each account, in memory only.
package state

import (
	"sync"
	"time"
)

// PendingTimeout is how long a probe waits for its usage record. Some requests,
// such as token counts, go through account selection without one; after this
// long the account is due a try again.
const PendingTimeout = 2 * time.Minute

// Answer is what one usage record said about an account.
type Answer struct {
	// ResetHeader reports whether the response carried the weekly-reset header.
	ResetHeader bool
	// Reset7d is the header's reset time when it was usable, otherwise zero.
	Reset7d time.Time
	// RateLimited reports whether the response was a 429.
	RateLimited bool
}

// View is what the pick rule needs to know about one account at one moment.
type View struct {
	// Reset7d is the last weekly reset time seen, or zero for none.
	Reset7d time.Time
	// Due means the next pick should try this account once.
	Due bool
}

// Store holds per-account state, keyed by auth ID. It is safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	accounts map[string]*account
}

type account struct {
	reset7d time.Time
	// observed: a response has been seen since start or since the last retest.
	observed bool
	// pending: picked as a probe at pendingSince and not answered yet.
	pending      bool
	pendingSince time.Time
	// probeDue: a retest asked for the next pick to try this account.
	probeDue bool
	// rateLimited: the last response was a 429.
	rateLimited bool
}

// New returns an empty store. Every account starts unobserved, so it is due.
func New() *Store {
	return &Store{accounts: make(map[string]*account)}
}

// Observe records a response for an account. When the response carried the
// weekly-reset header its value replaces the stored reset, and an unusable value
// clears it. Without the header the stored reset stays.
func (s *Store) Observe(id string, answer Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.get(id)
	if answer.ResetHeader {
		a.reset7d = answer.Reset7d
	}
	a.observed = true
	a.pending = false
	a.rateLimited = answer.RateLimited
}

// View returns the account's state at now.
func (s *Store) View(id string, now time.Time) View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(id, now)
}

// Pick offers choose a view of each id and returns the id it chose, or "" when it
// chose none of them. A chosen account that was due is marked as probed: it stops
// being due until it answers or PendingTimeout passes. It runs under the store's lock, so two concurrent
// picks never probe the same account.
func (s *Store) Pick(ids []string, now time.Time, choose func(map[string]View) string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make(map[string]View, len(ids))
	for _, id := range ids {
		views[id] = s.view(id, now)
	}
	id := choose(views)
	chosen, offered := views[id]
	if !offered {
		return ""
	}
	if chosen.Due {
		a := s.get(id)
		a.pending = true
		a.pendingSince = now
		a.probeDue = false
	}
	return id
}

// Retest is called after the account's cooldown was reset. When its last response
// was a 429 it becomes due again and Retest returns true.
func (s *Store) Retest(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok || !a.rateLimited {
		return false
	}
	a.rateLimited = false
	a.observed = false
	a.probeDue = true
	return true
}

func (s *Store) get(id string) *account {
	a, ok := s.accounts[id]
	if !ok {
		a = &account{}
		s.accounts[id] = a
	}
	return a
}

func (s *Store) view(id string, now time.Time) View {
	a, ok := s.accounts[id]
	if !ok {
		return View{Due: true}
	}
	resetPassed := !a.reset7d.IsZero() && !a.reset7d.After(now)
	waiting := a.pending && now.Sub(a.pendingSince) < PendingTimeout
	return View{
		Reset7d: a.reset7d,
		Due:     a.probeDue || (!waiting && (!a.observed || resetPassed)),
	}
}
