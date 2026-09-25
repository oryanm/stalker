package poller

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oryanm/stalker/internal/model"
)

// SettingMinInterval is the store setting holding the minimum update interval.
const SettingMinInterval = "min_interval"

const (
	minMinInterval = time.Minute
	// maxMinInterval keeps a mistyped limit from quietly stopping all updates
	maxMinInterval = 30 * 24 * time.Hour
	day            = 24 * time.Hour
)

// time.ParseDuration has no days, so a leading "Nd" is split off first
var daysPrefix = regexp.MustCompile(`^(\d+(?:\.\d+)?)d`)

// ParseInterval reads a typed interval such as "90m", "3h", "1h30m" or "2d",
// rounded to the minute. Empty or "0" means no limit and returns zero.
func ParseInterval(s string) (time.Duration, error) {
	in := strings.ToLower(strings.Join(strings.Fields(s), ""))
	if in == "" || in == "0" {
		return 0, nil
	}
	var d time.Duration
	rest := in
	if m := daysPrefix.FindStringSubmatch(in); m != nil {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, badInterval(s)
		}
		d = time.Duration(n * float64(day))
		rest = in[len(m[0]):]
	}
	if rest != "" {
		more, err := time.ParseDuration(rest)
		if err != nil {
			return 0, badInterval(s)
		}
		d += more
	}
	switch {
	case d < 0:
		return 0, badInterval(s)
	case d == 0:
		return 0, nil
	case d < minMinInterval:
		return 0, fmt.Errorf("the limit must be at least %s", FormatInterval(minMinInterval))
	case d > maxMinInterval:
		return 0, fmt.Errorf("the limit can be at most %s", FormatInterval(maxMinInterval))
	}
	return d.Round(time.Minute), nil
}

func badInterval(s string) error {
	return fmt.Errorf("%q is not an interval: type a number with a unit, like 30m, 3h, 1h30m or 2d", strings.TrimSpace(s))
}

// FormatInterval writes d the way ParseInterval reads it ("1d12h", "1h30m").
// Zero or negative is the empty string.
func FormatInterval(d time.Duration) string {
	d = d.Round(time.Minute)
	if d <= 0 {
		return ""
	}
	var b strings.Builder
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{day, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if n := d / u.unit; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.unit
		}
	}
	return b.String()
}

// EffectiveInterval is the tier's interval, raised to minInterval when that is longer.
func EffectiveInterval(imp model.Importance, minInterval time.Duration) time.Duration {
	return max(Interval(imp), minInterval)
}

// MinInterval returns the stored minimum update interval, zero when none is
// set. A value that cannot be read counts as no limit.
func (p *Poller) MinInterval(ctx context.Context) time.Duration {
	v, err := p.st.GetSetting(ctx, SettingMinInterval, "")
	if err != nil {
		slog.Warn("poller: read minimum interval", "err", err)
		return 0
	}
	d, err := ParseInterval(v)
	if err != nil {
		slog.Warn("poller: ignoring stored minimum interval", "value", v, "err", err)
		return 0
	}
	return d
}

// SetMinInterval stores the minimum update interval (zero removes it) and
// reschedules the follows whose interval it changes, so a lower limit takes
// effect at once rather than after each follow's next fetch.
func (p *Poller) SetMinInterval(ctx context.Context, d time.Duration) error {
	if d != 0 && (d < minMinInterval || d > maxMinInterval) {
		return fmt.Errorf("poller: minimum interval %s out of range", d)
	}
	old := p.MinInterval(ctx)
	if err := p.st.SetSetting(ctx, SettingMinInterval, FormatInterval(d)); err != nil {
		return fmt.Errorf("poller: %w", err)
	}
	if old == d {
		return nil
	}
	return p.reschedule(ctx, old, d)
}

func (p *Poller) reschedule(ctx context.Context, old, minInterval time.Duration) error {
	follows, err := p.st.ListFollows(ctx)
	if err != nil {
		return fmt.Errorf("poller: reschedule: %w", err)
	}
	next := map[int64]time.Time{}
	for _, f := range follows {
		// failing follows keep their backoff, never-fetched ones are due already, and in-flight ones record their own schedule
		if f.ErrorCount > 0 || f.LastFetchedAt.IsZero() || p.IsFetching(f.ID) {
			continue
		}
		interval := EffectiveInterval(f.Importance, minInterval)
		if interval == EffectiveInterval(f.Importance, old) {
			continue
		}
		next[f.ID] = nextFetch(f.LastFetchedAt, interval, 0, 0, p.jitter())
	}
	if err := p.st.SetNextFetch(ctx, next); err != nil {
		return fmt.Errorf("poller: reschedule: %w", err)
	}
	p.Kick()
	return nil
}
