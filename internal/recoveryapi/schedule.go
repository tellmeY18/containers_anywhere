package recoveryapi

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule supports bounded numeric UTC cron: lists, ranges, and steps in five
// fields. No macros, names, seconds, timezones or random/jitter expressions.
// DOM/DOW use standard cron OR semantics when both are restricted.
type Schedule struct {
	fields [5]map[int]bool
	wild   [5]bool
}

func ParseSchedule(expr string) (Schedule, error) {
	var s Schedule
	parts := strings.Fields(expr)
	if len(expr) > 128 || len(parts) != 5 {
		return s, errors.New("schedule requires five numeric UTC cron fields")
	}
	for _, ch := range expr {
		if !strings.ContainsRune("0123456789*/,- \t", ch) {
			return s, errors.New("cron accepts only numeric fields, ranges, lists and steps")
		}
	}
	mins, maxs := []int{0, 0, 1, 1, 0}, []int{59, 23, 31, 12, 7}
	for i, part := range parts {
		s.fields[i] = map[int]bool{}
		s.wild[i] = strings.HasPrefix(part, "*")
		for _, item := range strings.Split(part, ",") {
			pair := strings.Split(item, "/")
			if len(pair) > 2 {
				return s, errors.New("invalid cron step")
			}
			lo, hi, step := mins[i], maxs[i], 1
			if len(pair) == 2 {
				n, err := strconv.Atoi(pair[1])
				if err != nil || n < 1 || n > maxs[i]-mins[i]+1 {
					return s, errors.New("invalid cron step")
				}
				step = n
			}
			if pair[0] != "*" {
				r := strings.Split(pair[0], "-")
				if len(r) > 2 {
					return s, errors.New("invalid cron range")
				}
				n, err := strconv.Atoi(r[0])
				if err != nil {
					return s, errors.New("invalid cron number")
				}
				lo, hi = n, n
				if len(r) == 2 {
					n, err = strconv.Atoi(r[1])
					if err != nil {
						return s, err
					}
					hi = n
				} else if len(pair) == 2 {
					hi = maxs[i]
				}
			}
			if lo < mins[i] || hi > maxs[i] || lo > hi {
				return s, errors.New("cron value out of range")
			}
			for n := lo; n <= hi; n += step {
				v := n
				if i == 4 && v == 7 {
					v = 0
				}
				s.fields[i][v] = true
			}
		}
	}
	// Conservative lower bound, including the wrap to the next hour. Scheduling
	// more often than 15 minutes is rejected, even when hours would create gaps.
	last, first := -1, -1
	for n := 0; n < 60; n++ {
		if s.fields[0][n] {
			if first < 0 {
				first = n
			}
			if last >= 0 && n-last < 15 {
				return s, errors.New("minimum schedule interval is 15 minutes")
			}
			last = n
		}
	}
	if 60-last+first < 15 {
		return s, errors.New("minimum schedule interval is 15 minutes")
	}
	return s, nil
}

func (s Schedule) Matches(t time.Time) bool {
	t = t.UTC()
	if !s.fields[0][t.Minute()] || !s.fields[1][t.Hour()] || !s.fields[3][int(t.Month())] {
		return false
	}
	dom, dow := s.fields[2][t.Day()], s.fields[4][int(t.Weekday())]
	if !s.wild[2] && !s.wild[4] {
		return dom || dow
	}
	return dom && dow
}

// Due coalesces missed runs to the latest slot in a 24h window. A backwards
// clock never replays an observed slot; no unbounded catch-up storm is possible.
func (s Schedule) Due(now, after time.Time) (time.Time, bool) {
	now = now.UTC().Truncate(time.Minute)
	if !now.After(after) {
		return time.Time{}, false
	}
	for t, n := now, 0; n < 24*60 && t.After(after); t, n = t.Add(-time.Minute), n+1 {
		if s.Matches(t) {
			return t, true
		}
	}
	return time.Time{}, false
}

func (s PolicySpec) Validate(cluster bool) error {
	if err := validateScope(s.Scope, cluster); err != nil {
		return err
	}
	if _, err := ParseSchedule(s.Schedule); err != nil {
		return err
	}
	if s.KeepLast < 1 || s.KeepLast > 100 {
		return errors.New("keepLast must be 1..100")
	}
	if cluster && s.Capture != nil {
		return errors.New("cluster policies use the guest engine, not workload capture")
	}
	if !cluster {
		if s.Capture == nil {
			return errors.New("capture is required")
		}
		if err := s.Capture.Validate(); err != nil {
			return err
		}
	}
	if s.Destination != "" && !dnsName(s.Destination) {
		return fmt.Errorf("invalid destination")
	}
	return nil
}
