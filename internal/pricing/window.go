package pricing

import "time"

// matchesTimeWindow reports whether at falls inside one window. The interval is
// half-open [start,end); when end < start the window crosses midnight and the
// weekday is attributed to the day the window starts.
func matchesTimeWindow(at time.Time, window TimeWindow) bool {
	location, err := windowLocation(window.TZ)
	if err != nil {
		return false
	}
	start, err := parseClock(window.Start)
	if err != nil {
		return false
	}
	end, err := parseClock(window.End)
	if err != nil {
		return false
	}
	local := at.In(location)
	minutes := local.Hour()*60 + local.Minute()
	days := windowDays(window.Days)

	if end > start {
		return days[dayKey(local.Weekday())] && minutes >= start && minutes < end
	}
	// Cross-midnight: the tail belongs to the weekday the window started on.
	previous := local.AddDate(0, 0, -1)
	if minutes >= start {
		return days[dayKey(local.Weekday())]
	}
	if minutes < end {
		return days[dayKey(previous.Weekday())]
	}
	return false
}

// windowDays treats an empty day list as "every day".
func windowDays(days []string) map[string]bool {
	if len(days) == 0 {
		return map[string]bool{"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}
	}
	out := make(map[string]bool, len(days))
	for _, day := range days {
		out[normalizeDay(day)] = true
	}
	return out
}

func dayKey(weekday time.Weekday) string {
	switch weekday {
	case time.Monday:
		return "mon"
	case time.Tuesday:
		return "tue"
	case time.Wednesday:
		return "wed"
	case time.Thursday:
		return "thu"
	case time.Friday:
		return "fri"
	case time.Saturday:
		return "sat"
	default:
		return "sun"
	}
}

func normalizeDay(day string) string {
	switch day {
	case "Monday", "monday":
		return "mon"
	case "Tuesday", "tuesday":
		return "tue"
	case "Wednesday", "wednesday":
		return "wed"
	case "Thursday", "thursday":
		return "thu"
	case "Friday", "friday":
		return "fri"
	case "Saturday", "saturday":
		return "sat"
	case "Sunday", "sunday":
		return "sun"
	}
	return day
}

// windowLabel names the window that a rule matched, for the pricing snapshot.
func windowLabel(window TimeWindow) string {
	days := "every day"
	if len(window.Days) > 0 {
		days = ""
		for index, day := range window.Days {
			if index > 0 {
				days += ","
			}
			days += normalizeDay(day)
		}
	}
	tz := window.TZ
	if tz == "" {
		tz = "UTC"
	}
	return days + " " + window.Start + "-" + window.End + " " + tz
}
