// Package schedule translates human-readable schedule strings into cron
// expressions understood by robfig/cron/v3.
//
// Accepted forms (tried in order):
//  1. Standard 5-field cron: "0 20 * * 1-5"
//  2. Robfig descriptors: "@every 10m", "@daily", "@hourly"
//  3. Human DSL: "Weekdays at ~20:00", "Mondays at 9am", "Every 10 minutes"
package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Parse accepts a schedule string and returns the canonical cron expression
// plus a hasJitter flag indicating whether the "~" marker was present.
func Parse(s string) (cronExpr string, hasJitter bool, err error) {
	if _, err := cronParser.Parse(s); err == nil {
		return s, false, nil
	}

	stripped, hasJitter := stripJitter(s)

	cron, err := humanToCron(stripped)
	if err != nil {
		return "", false, fmt.Errorf("unrecognised schedule %q: %w", s, err)
	}

	if _, err := cronParser.Parse(cron); err != nil {
		return "", false, fmt.Errorf("internal schedule translation error for %q → %q: %w", s, cron, err)
	}
	return cron, hasJitter, nil
}

// Describe returns a human-readable description of the parsed schedule.
func Describe(cronExpr string, hasJitter bool, jitter string) string {
	base := cronExpr
	if hasJitter && jitter != "" {
		base += " (±" + jitter + " jitter)"
	}
	return base
}

// jitterRe matches optional ~ before a time component like "~20:00" or "~9am"
var jitterRe = regexp.MustCompile(`~(\d)`)

func stripJitter(s string) (string, bool) {
	if !strings.Contains(s, "~") {
		return s, false
	}
	return jitterRe.ReplaceAllString(s, "$1"), true
}

// dayNames maps day names to their cron day-of-week numbers.
var dayNames = map[string]string{
	"sunday": "0", "sun": "0",
	"monday": "1", "mon": "1",
	"tuesday": "2", "tue": "2",
	"wednesday": "3", "wed": "3",
	"thursday": "4", "thu": "4",
	"friday": "5", "fri": "5",
	"saturday": "6", "sat": "6",
}

var everyRe2 = regexp.MustCompile(`(?i)^every\s+(\d+)\s+(minutes?|mins?|m|hours?|hrs?|h)$`)

func humanToCron(s string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(s))

	// "every N minutes/hours"
	if m := everyRe2.FindStringSubmatch(lower); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := strings.ToLower(m[2])
		if strings.HasPrefix(unit, "h") {
			return fmt.Sprintf("@every %dh", n), nil
		}
		return fmt.Sprintf("@every %dm", n), nil
	}

	// "at" form: "<freq> at <time>"
	freqPart, timePart, ok := strings.Cut(lower, " at ")
	if !ok {
		return "", fmt.Errorf("expected \"at\" keyword")
	}
	freqPart = strings.TrimSpace(freqPart)
	timePart = strings.TrimSpace(timePart)

	dow, err := parseDow(freqPart)
	if err != nil {
		return "", err
	}
	hour, minute, err := parseTime(timePart)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d %d * * %s", minute, hour, dow), nil
}

// parseDow converts a frequency word/list into cron day-of-week field.
func parseDow(freq string) (string, error) {
	switch freq {
	case "daily", "every day":
		return "*", nil
	case "weekdays", "weekday", "every weekday":
		return "1-5", nil
	case "weekends", "weekend", "every weekend":
		return "0,6", nil
	}

	// Comma-separated day list: "mon, wed, fri"
	parts := strings.Split(freq, ",")
	if len(parts) == 1 {
		// Single day, possibly pluralised: "mondays"
		d := strings.TrimSuffix(strings.TrimSpace(parts[0]), "s")
		if n, ok := dayNames[d]; ok {
			return n, nil
		}
		return "", fmt.Errorf("unknown frequency %q", freq)
	}
	nums := make([]string, 0, len(parts))
	for _, p := range parts {
		d := strings.TrimSuffix(strings.TrimSpace(p), "s")
		n, ok := dayNames[d]
		if !ok {
			return "", fmt.Errorf("unknown day name %q in %q", p, freq)
		}
		nums = append(nums, n)
	}
	return strings.Join(nums, ","), nil
}

// parseTime parses time strings like "20:00", "9am", "9:30pm", "noon", "midnight".
func parseTime(t string) (hour, minute int, err error) {
	t = strings.TrimSpace(t)
	switch t {
	case "noon":
		return 12, 0, nil
	case "midnight":
		return 0, 0, nil
	}

	isPM := strings.HasSuffix(t, "pm")
	isAM := strings.HasSuffix(t, "am")
	if isPM || isAM {
		t = t[:len(t)-2]
	}
	t = strings.TrimSpace(t)

	var h, m int
	if hStr, mStr, found := strings.Cut(t, ":"); found {
		h, err = strconv.Atoi(hStr)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid hour in %q", t)
		}
		m, err = strconv.Atoi(mStr)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid minute in %q", t)
		}
	} else {
		h, err = strconv.Atoi(t)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid time %q", t)
		}
	}

	if isPM && h < 12 {
		h += 12
	}
	if isAM && h == 12 {
		h = 0
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time out of range: %d:%02d", h, m)
	}
	return h, m, nil
}
