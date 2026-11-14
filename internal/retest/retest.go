// Package retest clears the gateway's cooldown records so blocked accounts are tried again.
package retest

import (
	"fmt"
	"sync"
	"time"

	"github.com/rawphp/cpa-soonest-reset-plugin/internal/pick"
)

// Auth is one credential from host.auth.list. Scheduler candidates and usage
// records name an account by ID; the cooldown reset takes its auth index.
type Auth struct {
	ID        string
	AuthIndex string
	Provider  string
}

// Host is the part of the gateway a retest pass needs.
type Host interface {
	ListAuths() ([]Auth, error)
	ResetCooldown(authIndex string) error
}

// Marker decides, after an account's cooldown was reset, whether the next pick
// should try it. state.Store satisfies it.
type Marker interface {
	Retest(id string) bool
}

// Result says what one pass did.
type Result struct {
	// Reset lists the auth IDs whose cooldown was reset.
	Reset []string
	// Marked lists the auth IDs the next picks will try.
	Marked []string
	// Errors holds every host error met; a failed account does not stop the pass.
	Errors []error
}

// Pass resets the cooldown of every Claude account and marks those the store says
// should be tried again.
func Pass(host Host, marker Marker) Result {
	var result Result
	auths, err := host.ListAuths()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list accounts: %w", err))
		return result
	}
	for _, auth := range auths {
		if !pick.IsClaude(auth.Provider) {
			continue
		}
		if err := host.ResetCooldown(auth.AuthIndex); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("reset %s: %w", auth.ID, err))
			continue
		}
		result.Reset = append(result.Reset, auth.ID)
		if marker.Retest(auth.ID) {
			result.Marked = append(result.Marked, auth.ID)
		}
	}
	return result
}

// Schedule says when a pass is due: at once, then once per interval. An
// interval of zero or less turns passes off. It is safe for concurrent use.
type Schedule struct {
	mu       sync.Mutex
	interval time.Duration
	ran      bool
	last     time.Time
}

// NewSchedule returns a schedule whose first pass is due straight away.
func NewSchedule(interval time.Duration) *Schedule {
	return &Schedule{interval: interval}
}

// SetInterval changes the interval and keeps the time of the last pass.
func (s *Schedule) SetInterval(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interval = interval
}

// Due reports whether a pass should run at now. When it returns true the caller
// owns that pass: the next one is due a full interval later.
func (s *Schedule) Due(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interval <= 0 || (s.ran && now.Sub(s.last) < s.interval) {
		return false
	}
	s.ran = true
	s.last = now
	return true
}
