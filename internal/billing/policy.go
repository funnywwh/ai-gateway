package billing

// In-flight decisions taken while a call is streaming.
const (
	InflightContinue = "continue"
	InflightWarn     = "warn"
	InflightThrottle = "throttle"
	InflightAbort    = "abort"
)

// Default ratios used when the configuration leaves them unset.
const (
	DefaultSoftRatio = 0.8
	DefaultHardRatio = 1.0
)

// InflightState is what the mid-stream check knows.
type InflightState struct {
	// Policy is warn|throttle|abort|allow_overdraft.
	Policy string
	// AccruedMicros is the charge for the usage metered so far.
	AccruedMicros int64
	// LimitMicros is what this call may consume: the reservation, plus the
	// overdraft allowance for accounts whose policy allows one.
	LimitMicros int64
	// SoftRatio triggers the warning band; HardRatio ends the call.
	SoftRatio float64
	HardRatio float64
}

// DecideInflight maps the current spend to an action.
//
// The soft band only ever warns or throttles; the hard band ends the call for every
// policy except "warn", whose whole point is to let the request finish and be
// reconciled later. Throttling is implemented as backpressure, so reaching the hard
// limit after throttling means the money did not arrive and the call must stop.
func DecideInflight(state InflightState) string {
	limit := state.LimitMicros
	if limit <= 0 {
		return InflightContinue
	}
	soft := state.SoftRatio
	if soft <= 0 || soft > 1 {
		soft = DefaultSoftRatio
	}
	hard := state.HardRatio
	if hard <= 0 || hard < soft {
		hard = DefaultHardRatio
	}
	ratio := float64(state.AccruedMicros) / float64(limit)
	if ratio >= hard {
		if state.Policy == "warn" {
			return InflightWarn
		}
		return InflightAbort
	}
	if ratio >= soft {
		switch state.Policy {
		case "throttle":
			return InflightThrottle
		case "allow_overdraft":
			return InflightContinue
		default:
			return InflightWarn
		}
	}
	return InflightContinue
}

// InflightLimit is the spend a single call may reach before the policy applies.
func InflightLimit(policy string, reservedMicros, availableMicros int64) int64 {
	limit := reservedMicros
	if policy == "allow_overdraft" && availableMicros > 0 {
		limit += availableMicros
	}
	return limit
}
