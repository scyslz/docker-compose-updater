package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSchedule implements standard five-field Unix cron syntax, including
// lists, ranges, steps, and month/day names. It intentionally has no external
// dependencies so the updater can build in offline/minimal environments.
type cronSchedule struct {
	minute, hour, day, month, weekday map[int]bool
	dayWildcard, weekdayWildcard      bool
}

func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d", len(fields))
	}
	mins, _, err := parseCronField(fields[0], 0, 59, nil, false)
	if err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	hours, _, err := parseCronField(fields[1], 0, 23, nil, false)
	if err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	days, dayWild, err := parseCronField(fields[2], 1, 31, nil, false)
	if err != nil {
		return nil, fmt.Errorf("day-of-month: %w", err)
	}
	months, _, err := parseCronField(fields[3], 1, 12, map[string]int{
		"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	}, false)
	if err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	weekdays, weekdayWild, err := parseCronField(fields[4], 0, 7, map[string]int{
		"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
	}, true)
	if err != nil {
		return nil, fmt.Errorf("day-of-week: %w", err)
	}
	// Cron permits both 0 and 7 to represent Sunday.
	if weekdays[7] {
		weekdays[0] = true
		delete(weekdays, 7)
	}
	return &cronSchedule{minute: mins, hour: hours, day: days, month: months, weekday: weekdays, dayWildcard: dayWild, weekdayWildcard: weekdayWild}, nil
}

func parseCronField(field string, min, max int, names map[string]int, sundayAlias bool) (map[int]bool, bool, error) {
	allowed := map[int]bool{}
	wildcard := field == "*"
	for _, item := range strings.Split(strings.ToUpper(field), ",") {
		if item == "" {
			return nil, false, fmt.Errorf("empty list item")
		}
		base, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return nil, false, fmt.Errorf("invalid step %q", stepText)
			}
			step = n
		}
		start, end := min, max
		if base != "*" {
			parts := strings.Split(base, "-")
			if len(parts) > 2 {
				return nil, false, fmt.Errorf("invalid range %q", base)
			}
			var err error
			start, err = cronValue(parts[0], names)
			if err != nil {
				return nil, false, err
			}
			end = start
			if len(parts) == 2 {
				end, err = cronValue(parts[1], names)
				if err != nil {
					return nil, false, err
				}
			} else if hasStep {
				// In cron, N/step means N through the field maximum by step.
				end = max
			}
		}
		if start < min || end > max || start > end {
			return nil, false, fmt.Errorf("value/range %q outside %d-%d", item, min, max)
		}
		for n := start; n <= end; n += step {
			allowed[n] = true
		}
	}
	if len(allowed) == 0 {
		return nil, false, fmt.Errorf("field selects no values")
	}
	return allowed, wildcard, nil
}

func cronValue(s string, names map[string]int) (int, error) {
	if n, ok := names[s]; ok {
		return n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return n, nil
}

func (c *cronSchedule) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	dom := c.day[t.Day()]
	dow := c.weekday[int(t.Weekday())]
	// Traditional cron semantics: if both day fields are restricted, either
	// field may match. If one is wildcard, the other controls the date.
	if c.dayWildcard && c.weekdayWildcard {
		return dom && dow
	}
	if c.dayWildcard {
		return dow
	}
	if c.weekdayWildcard {
		return dom
	}
	return dom || dow
}

func (c *cronSchedule) Next(after time.Time) time.Time {
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	// A valid five-field cron expression should match within five years.
	limit := candidate.AddDate(5, 0, 0)
	for candidate.Before(limit) {
		if c.matches(candidate) {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}
}
