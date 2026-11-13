// Package state keeps what the plugin knows about each account, in memory only.
package state

import (
	"sync"
	"time"
)

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
	// pending: picked as a probe and not answered yet.
	pending bool
	// probeDue: a retest asked for the next pick to try this account.
	probeDue bool
	// rateLimited: the last response was a 429.
	rateLimited bool
}

// New returns an empty store. Every account starts unobserved, so it is due.
func New() *Store {
	return &Store{accounts: make(map[string]*account)}
}

// Observe records a response for an account. A zero reset7d clears the stored reset.
func (s *Store) Observe(id string, reset7d time.Time, rateLimited bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.get(id)
	a.reset7d = reset7d
	a.observed = true
	a.pending = false
	a.rateLimited = rateLimited
}

// View returns the account's state at now.
func (s *Store) View(id string, now time.Time) View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(id, now)
}

// Pick offers choose a view of each id and returns the id it chose, or "" when it
// chose none of them. A chosen account that was due is marked as probed: it stops
// being due until it answers. It runs under the store's lock, so two concurrent
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
	return View{
		Reset7d: a.reset7d,
		Due:     a.probeDue || (!a.pending && (!a.observed || resetPassed)),
	}
}
