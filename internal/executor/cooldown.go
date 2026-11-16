package executor

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxParsedCooldown = 30 * 24 * time.Hour

const unitAlt = `(?:milliseconds?|msecs?|ms|days?|d|hours?|hrs?|hr|h|minutes?|mins?|min|m|seconds?|secs?|sec|s)`
const durAtom = `[0-9]+(?:\.[0-9]+)?\s*` + unitAlt

var (
	reCooldownContext = regexp.MustCompile(`(?i)(?:resets?\s+in|try\s+again\s+in|retry\s+(?:in|after)|retry[-_ ]delay|available\s+in|wait)[\s"':=]+(` + durAtom + `(?:(?:\s*,\s*|\s+and\s+|\s*)` + durAtom + `)*)`)
	reUnitChunk       = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(` + unitAlt + `)`)
	reISO8601         = regexp.MustCompile(`\b20[0-9]{2}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}(?::[0-9]{2})?(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:?[0-9]{2})\b`)
	reResetDateText   = regexp.MustCompile(`(?i)(?:resets?\s+at|try\s+again\s+at|until)\s+([A-Za-z]{3,9}\s+[0-9]{1,2},?\s+20[0-9]{2}\s+[0-9]{1,2}:[0-9]{2}(?::[0-9]{2})?(?:\s*(?:UTC|GMT|[+-][0-9]{2}:?[0-9]{2}))?)`)
)

// ParseCooldown extracts a retry/reset duration from an HTTP Retry-After header and/or CLI/API response text.
// Returns (duration, source) where source is "parsed" if extracted from response metadata or "default" when falling back.
func ParseCooldown(text string, headers http.Header, fallback time.Duration) (time.Duration, string) {
	return parseCooldownAt(text, headers, fallback, time.Now())
}

func parseCooldownAt(text string, headers http.Header, fallback time.Duration, now time.Time) (time.Duration, string) {
	if headers != nil {
		if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
			if sec, err := strconv.ParseFloat(raw, 64); err == nil && sec > 0 {
				return clampCooldown(time.Duration(math.Ceil(sec)) * time.Second), "parsed"
			}
			if t, err := http.ParseTime(raw); err == nil {
				if d := t.Sub(now); d > 0 {
					return clampCooldown(d), "parsed"
				}
			}
		}
	}

	if d := parseDurationFromText(text, now); d > 0 {
		return clampCooldown(d), "parsed"
	}

	if fallback > 0 {
		return fallback, "default"
	}
	return 0, "default"
}

func parseDurationFromText(text string, now time.Time) time.Duration {
	if strings.TrimSpace(text) == "" {
		return 0
	}

	if m := reCooldownContext.FindStringSubmatch(text); len(m) >= 2 {
		if d := parseCompoundDuration(m[1]); d > 0 {
			return d
		}
	}

	for _, rawISO := range reISO8601.FindAllString(text, -1) {
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05Z07:00",
			"2006-01-02 15:04Z07:00",
			"2006-01-02T15:04Z",
		} {
			if t, err := time.Parse(layout, rawISO); err == nil {
				if d := t.Sub(now); d > 0 {
					return d
				}
			}
		}
	}

	if m := reResetDateText.FindStringSubmatch(text); len(m) >= 2 {
		candidate := strings.ReplaceAll(strings.TrimSpace(m[1]), ",", "")
		for _, layout := range []string{
			"Jan 2 2006 15:04:05 MST",
			"Jan 2 2006 15:04 MST",
			"January 2 2006 15:04:05 MST",
			"January 2 2006 15:04 MST",
		} {
			if t, err := time.Parse(layout, candidate); err == nil {
				if d := t.Sub(now); d > 0 {
					return d
				}
			}
		}
	}

	return 0
}

func parseCompoundDuration(expr string) time.Duration {
	matches := reUnitChunk.FindAllStringSubmatch(expr, -1)
	if len(matches) == 0 {
		return 0
	}
	var total time.Duration
	for _, m := range matches {
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil || val <= 0 {
			continue
		}
		unit := strings.ToLower(m[2])
		var mult time.Duration
		switch {
		case strings.HasPrefix(unit, "ms") || strings.HasPrefix(unit, "millis") || strings.HasPrefix(unit, "msec"):
			mult = time.Millisecond
		case strings.HasPrefix(unit, "d"):
			mult = 24 * time.Hour
		case strings.HasPrefix(unit, "h"):
			mult = time.Hour
		case strings.HasPrefix(unit, "m"):
			mult = time.Minute
		case strings.HasPrefix(unit, "s"):
			mult = time.Second
		}
		total += time.Duration(math.Ceil(val * float64(mult)))
	}
	return total
}

func clampCooldown(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	if d > maxParsedCooldown {
		return maxParsedCooldown
	}
	return d
}
