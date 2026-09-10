// Package backup produces consistent SQLite snapshots on a schedule, verifies them
// and prunes old copies. It never blocks the request path: VACUUM INTO runs in its own
// read transaction and writers keep going.
package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// Schedule is a parsed cron expression (minute hour day month weekday).
type Schedule struct {
	raw      string
	minute   [60]bool
	hour     [24]bool
	day      [32]bool
	month    [13]bool
	weekday  [7]bool
	dayAny   bool
	weekdAny bool
}

// ParseSchedule parses the five-field cron subset the configuration uses: \"*\", lists
// (a,b), ranges (a-b) and steps (*/n, a-b/n).
func ParseSchedule(expression string) (*Schedule, error) {
	fields := strings.Fields(strings.TrimSpace(expression))
	if len(fields) != 5 {
		return nil, domain.ErrInvalidRequest("cron expression must have five fields: minute hour day month weekday")
	}
	schedule := &Schedule{raw: expression}
	if err := fill(schedule.minute[:], fields[0], 0, 59, "minute"); err != nil {
		return nil, err
	}
	if err := fill(schedule.hour[:], fields[1], 0, 23, "hour"); err != nil {
		return nil, err
	}
	if err := fill(schedule.day[1:], fields[2], 1, 31, "day"); err != nil {
		return nil, err
	}
	if err := fill(schedule.month[1:], expandNames(fields[3], monthNames), 1, 12, "month"); err != nil {
		return nil, err
	}
	if err := fill(schedule.weekday[:], expandNames(fields[4], weekdayNames), 0, 6, "weekday"); err != nil {
		return nil, err
	}
	schedule.dayAny = fields[2] == "*"
	schedule.weekdAny = fields[4] == "*"
	return schedule, nil
}

func fill(target []bool, field string, min, max int, label string) error {
	for _, part := range strings.Split(field, ",") {
		step := 1
		if index := strings.Index(part, "/"); index >= 0 {
			parsed, err := strconv.Atoi(part[index+1:])
			if err != nil || parsed <= 0 {
				return domain.ErrInvalidRequest("cron " + label + " has an invalid step")
			}
			step = parsed
			part = part[:index]
		}
		start, end := min, max
		switch {
		case part == "*" || part == "":
		case strings.Contains(part, "-"):
			bounds := strings.SplitN(part, "-", 2)
			var err error
			start, err = strconv.Atoi(bounds[0])
			if err != nil {
				return domain.ErrInvalidRequest("cron " + label + " has an invalid range")
			}
			end, err = strconv.Atoi(bounds[1])
			if err != nil {
				return domain.ErrInvalidRequest("cron " + label + " has an invalid range")
			}
		default:
			value, err := strconv.Atoi(part)
			if err != nil {
				return domain.ErrInvalidRequest("cron " + label + " has an invalid value")
			}
			start, end = value, value
		}
		if start < min || end > max || start > end {
			return domain.ErrInvalidRequest(fmt.Sprintf("cron %s value %d-%d is out of range %d-%d", label, start, end, min, max))
		}
		for value := start; value <= end; value += step {
			target[value-min] = true
		}
	}
	return nil
}

// String returns the original expression.
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	return s.raw
}

// Next returns the first matching instant strictly after the given time.
func (s *Schedule) Next(after time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}
	// Minute granularity: start at the next minute boundary in the same location.
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	deadline := candidate.AddDate(2, 0, 0)
	for candidate.Before(deadline) {
		if !s.month[candidate.Month()] {
			// Jump to the first day of the next month.
			candidate = time.Date(candidate.Year(), candidate.Month()+1, 1, 0, 0, 0, 0, candidate.Location())
			continue
		}
		if !s.matchesDay(candidate) {
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, 0, 0, 0, 0, candidate.Location())
			continue
		}
		if !s.hour[candidate.Hour()] {
			candidate = candidate.Add(time.Hour).Truncate(time.Hour)
			continue
		}
		if !s.minute[candidate.Minute()] {
			candidate = candidate.Add(time.Minute)
			continue
		}
		return candidate
	}
	return time.Time{}
}

// matchesDay implements the standard cron rule: when both day-of-month and
// day-of-week are restricted, a date matches if either one does.
func (s *Schedule) matchesDay(at time.Time) bool {
	dayOK := s.day[at.Day()]
	weekdayOK := s.weekday[int(at.Weekday())]
	switch {
	case s.dayAny && s.weekdAny:
		return true
	case s.dayAny:
		return weekdayOK
	case s.weekdAny:
		return dayOK
	default:
		return dayOK || weekdayOK
	}
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var weekdayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// expandNames rewrites cron names (mon, jan, ...) into their numeric values so the
// numeric parser can stay simple. Names may appear in lists, ranges and steps.
func expandNames(field string, names map[string]int) string {
	parts := strings.Split(field, ",")
	for index, part := range parts {
		step := ""
		if slash := strings.Index(part, "/"); slash >= 0 {
			step = part[slash:]
			part = part[:slash]
		}
		pieces := strings.Split(part, "-")
		for position, piece := range pieces {
			if value, ok := names[strings.ToLower(strings.TrimSpace(piece))]; ok {
				pieces[position] = strconv.Itoa(value)
			}
		}
		parts[index] = strings.Join(pieces, "-") + step
	}
	return strings.Join(parts, ",")
}
