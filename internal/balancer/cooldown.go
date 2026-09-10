package balancer

import "time"

// Cooldowns tracks temporary exclusions (upstream quota exhaustion, operator pauses).
// They are separate from the circuit breaker: a cooldown has an explicit deadline and
// does not depend on observed failures.
type cooldownState struct {
	until time.Time
	note  string
}

// SetCooldown excludes a target until the given deadline.
func (s *State) SetCooldown(key string, until time.Time, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooldowns == nil {
		s.cooldowns = map[string]cooldownState{}
	}
	s.cooldowns[key] = cooldownState{until: until.UTC(), note: note}
}

// ClearCooldown removes an exclusion.
func (s *State) ClearCooldown(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cooldowns, key)
}

// CoolingDown reports whether key is excluded, and until when.
func (s *State) CoolingDown(key string, now time.Time) (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.cooldowns[key]
	if !ok {
		return false, time.Time{}
	}
	if now.After(cs.until) {
		delete(s.cooldowns, key)
		return false, time.Time{}
	}
	return true, cs.until
}

// Cooldowns returns every active exclusion (diagnostics).
func (s *State) Cooldowns(now time.Time) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for key, cs := range s.cooldowns {
		if now.After(cs.until) {
			delete(s.cooldowns, key)
			continue
		}
		out[key] = cs.until
	}
	return out
}
