package normalize

import (
	"strings"
	"testing"
	"time"
)

func TestStripInvisible(t *testing.T) {
	stega := strings.Repeat("\u200b\u200c\ufeff\u200d", 280)
	tests := []struct{ in, want string }{
		{"Databricks Liquid Clustering Simplified" + stega, "Databricks Liquid Clustering Simplified"},
		{"a\u200b\u200bb", "ab"},
		{"\ufeffBOM", "BOM"},                   // lone U+FEFF always goes
		{"👩\u200d💻 coder", "👩\u200d💻 coder"},   // lone ZWJ in an emoji sequence stays
		{"soft\u200bbreak", "soft\u200bbreak"}, // a lone zero-width space is left alone
		{"x\u2060\u2064y", "xy"},
		{"plain", "plain"},
	}
	for _, tt := range tests {
		if got := StripInvisible(tt.in); got != tt.want {
			t.Errorf("StripInvisible(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText("  Loop\n\t engineering \u200b\u200b "); got != "Loop engineering" {
		t.Errorf("got %q", got)
	}
}

func TestParseDate(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	tests := []struct {
		in       string
		layouts  []string
		want     string // RFC 3339
		dateOnly bool
	}{
		{"2026-09-21T15:12:00.000Z", nil, "2026-09-21T15:12:00Z", false},
		{"2026-09-21T08:12:00-07:00", nil, "2026-09-21T08:12:00-07:00", false},
		{"2026-09-21T15:12:00", nil, "2026-09-21T15:12:00-07:00", false}, // no zone: site timezone
		{"2026-09-22", nil, "2026-09-22T00:00:00-07:00", true},
		{"Mon, 21 Sep 2026 15:12:00 GMT", nil, "2026-09-21T15:12:00Z", false},
		{"Mon, 21 Sep 2026 15:12:00 +0000", nil, "2026-09-21T15:12:00Z", false},
		{"Sep 17, 2026", []string{"Jan 2, 2006"}, "2026-09-17T00:00:00-07:00", true},
		{"Aug 07, 2026", []string{"Jan 2, 2006"}, "2026-08-07T00:00:00-07:00", true},
		{"September 17, 2026", []string{"Jan 2, 2006", "January 2, 2006"}, "2026-09-17T00:00:00-07:00", true},
		{"Monday, September 21, 2026", []string{"Monday, January 2, 2006"}, "2026-09-21T00:00:00-07:00", true},
		{"2026-09-22 15", []string{"2006-01-02 15"}, "2026-09-22T15:00:00-07:00", false},
		{"Sep 22, 2026 3:4 PM", []string{"Jan 2, 2006 3:4 PM"}, "2026-09-22T15:04:00-07:00", false},
		// DST edge: standard time applies in December.
		{"Dec 1, 2026", []string{"Jan 2, 2006"}, "2026-12-01T00:00:00-08:00", true},
	}
	for _, tt := range tests {
		d, err := ParseDate(tt.in, tt.layouts, la)
		if err != nil {
			t.Errorf("%q: %v", tt.in, err)
			continue
		}
		if got := d.Time.Format(time.RFC3339); got != tt.want && !d.Time.Equal(mustTime(t, tt.want)) {
			t.Errorf("%q: got %s, want %s", tt.in, got, tt.want)
		}
		if d.DateOnly != tt.dateOnly {
			t.Errorf("%q: DateOnly = %v, want %v", tt.in, d.DateOnly, tt.dateOnly)
		}
	}
	for _, bad := range []string{"", "yesterday", "Sep 17, 2026"} { // last: no layout configured
		if _, err := ParseDate(bad, nil, la); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
