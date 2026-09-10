package balancer

// The cooldown map is a lazily-initialised field of State (see cooldown.go).
// Keeping it lazy means State.New stays allocation-light on the hot path.
