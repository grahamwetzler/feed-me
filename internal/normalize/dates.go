package normalize

import (
	"fmt"
	"time"
)

// builtinLayouts are tried before a site's configured layouts (§5.1):
// RFC 3339 / ISO 8601 forms, then the RFC 1123/822 family used by RSS.
var builtinLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST",
	time.RFC822Z,
	time.RFC822,
	time.RFC850,
}

// Date is a parsed timestamp. DateOnly means the source had no time of day,
// so the §5.2 time-of-day policy applies.
type Date struct {
	Time     time.Time
	DateOnly bool
	Layout   string
}

// ParseDate parses a date with the built-in layouts and then the given ones.
// Values without a zone are read in loc.
func ParseDate(value string, layouts []string, loc *time.Location) (Date, error) {
	v := CleanText(value)
	if v == "" {
		return Date{}, fmt.Errorf("empty date")
	}
	if loc == nil {
		loc = time.UTC
	}
	for _, list := range [][]string{builtinLayouts, layouts} {
		for _, l := range list {
			t, err := time.ParseInLocation(l, v, loc)
			if err != nil {
				continue
			}
			return Date{Time: t, DateOnly: !hasClock(l), Layout: l}, nil
		}
	}
	return Date{}, fmt.Errorf("unrecognized date %q (add a matching Go layout to the field's layouts)", v)
}

// hasClock reports whether a layout carries a time of day: formatting two
// instants on the same day through it gives different text only if it does.
func hasClock(layout string) bool {
	a := time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC)
	b := time.Date(2001, 2, 3, 13, 14, 15, 0, time.UTC)
	return a.Format(layout) != b.Format(layout)
}
