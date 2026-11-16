package executor

import (
	"math"
	"net/http"
	"testing"
	"time"
)

func TestParseCooldown(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	fallback := 300 * time.Second

	cases := []struct {
		name       string
		text       string
		retryAfter string
		wantSec    int64
		wantSource string
	}{
		{
			name:       "agy compound duration",
			text:       "Individual quota reached for Gemini 2.5 Pro. Resets in 43h17m35s.",
			wantSec:    43*3600 + 17*60 + 35,
			wantSource: "parsed",
		},
		{
			name:       "gemini fractional seconds",
			text:       "RESOURCE_EXHAUSTED: Quota exceeded. Please retry in 38.412s.",
			wantSec:    39,
			wantSource: "parsed",
		},
		{
			name:       "codex hours minutes words",
			text:       "You've hit your usage limit. Try again in 14 hours 22 minutes.",
			wantSec:    14*3600 + 22*60,
			wantSource: "parsed",
		},
		{
			name:       "days and hours",
			text:       "Rate limit hit. Resets in 1d 2h 30m",
			wantSec:    24*3600 + 2*3600 + 30*60,
			wantSource: "parsed",
		},
		{
			name:       "ISO timestamp",
			text:       "Usage limit reached. Resets at 2026-11-16T14:30:00Z.",
			wantSec:    2*3600 + 30*60,
			wantSource: "parsed",
		},
		{
			name:       "human UTC timestamp",
			text:       "Rate limit resets at Nov 16, 2026 15:00 UTC.",
			wantSec:    3 * 3600,
			wantSource: "parsed",
		},
		{
			name:       "http Retry-After integer",
			text:       "Too many requests",
			retryAfter: "95",
			wantSec:    95,
			wantSource: "parsed",
		},
		{
			name:       "http Retry-After RFC1123 date",
			text:       "Too many requests",
			retryAfter: now.Add(180 * time.Second).Format(http.TimeFormat),
			wantSec:    180,
			wantSource: "parsed",
		},
		{
			name:       "fallback when no duration in response",
			text:       "code 429 rate limit reached, try later",
			wantSec:    300,
			wantSource: "default",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hdr http.Header
			if tc.retryAfter != "" {
				hdr = http.Header{"Retry-After": []string{tc.retryAfter}}
			}
			dur, src := parseCooldownAt(tc.text, hdr, fallback, now)
			if src != tc.wantSource {
				t.Fatalf("source: got %q want %q (dur=%v)", src, tc.wantSource, dur)
			}
			gotSec := int64(math.Ceil(dur.Seconds()))
			if gotSec != tc.wantSec {
				t.Fatalf("seconds: got %d want %d (text=%q)", gotSec, tc.wantSec, tc.text)
			}
		})
	}
}

func TestClassifyCLIErrorExtractsRateLimitCooldown(t *testing.T) {
	err := ClassifyCLIError("antigravity", "Error: Individual quota reached. Resets in 2h15m.", nil, 1)
	if err == nil || err.Kind != ErrRateLimit {
		t.Fatalf("expected ErrRateLimit, got %+v", err)
	}
	wantSec := int64(2*3600 + 15*60)
	if err.RetryAfterSeconds != wantSec || err.CooldownSource != "parsed" {
		t.Fatalf("expected RetryAfterSeconds=%d parsed, got %+v", wantSec, err)
	}
}
