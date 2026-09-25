package opml

import (
	"strconv"
	"strings"
	"time"
)

// createdLayouts are tried in order by parseCreated.
var createdLayouts = []string{
	"Mon Jan 02 2006 15:04:05 GMT-0700", // JavaScript Date.toString() without the zone name
	time.RFC3339,                        // also JavaScript toISOString(), since parsing accepts fractional seconds
	// the RFC 1123 and 822 layouts below use "2" so days without a leading zero parse too
	"Mon, 2 Jan 2006 15:04:05 -0700", // RFC 1123Z
	"Mon, 2 Jan 2006 15:04:05 MST",   // RFC 1123
	"2 Jan 06 15:04 -0700",           // RFC 822Z
	"2 Jan 06 15:04 MST",             // RFC 822
}

// rfc822Zones are the North American zone names RFC 822 defines. time.Parse
// only knows abbreviations of the target location and gives others offset zero.
var rfc822Zones = map[string]int{
	"EST": -5 * 3600, "EDT": -4 * 3600,
	"CST": -6 * 3600, "CDT": -5 * 3600,
	"MST": -7 * 3600, "MDT": -6 * 3600,
	"PST": -8 * 3600, "PDT": -7 * 3600,
}

// parseCreated parses an outline's created attribute as UTC, zero when blank or unparseable.
func parseCreated(s string) time.Time {
	s = strings.TrimSpace(s)
	// Date.toString() appends a localized zone name such as "(Eastern Daylight Time)"
	if i := strings.IndexByte(s, '('); i > 0 && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return time.Time{}
	}
	// JavaScript exporters that serialize a Date as a number write epoch milliseconds
	if isDigits(s) {
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.UnixMilli(ms).UTC()
		}
		return time.Time{}
	}
	for _, layout := range createdLayouts {
		t, err := time.ParseInLocation(layout, s, time.UTC)
		if err != nil {
			continue
		}
		if name, offset := t.Zone(); offset == 0 {
			if off, ok := rfc822Zones[name]; ok {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(),
					time.FixedZone(name, off))
			}
		}
		return t.UTC()
	}
	return time.Time{}
}
