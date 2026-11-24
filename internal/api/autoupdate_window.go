package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // the hub's image has no time zone files of its own
)

// Automatic updates run in a maintenance window: on the chosen days, at the
// chosen time, for at most MaxMinutes. Everything is kept in one settings
// row (settingAutoUpdates) as JSON, so a change is saved all at once.

const settingAutoUpdates = "auto_updates"

type autoSettings struct {
	// Enabled is the global switch: off pauses every scheduled window (the
	// opt-ins stay as they are; "Run now" still works).
	Enabled bool     `json:"enabled"`
	Days    []string `json:"days"` // mon … sun
	Time    string   `json:"time"` // HH:MM, 24-hour, in TimeZone
	// TimeZone is an IANA name such as Europe/Berlin; "" means the hub's
	// local time (often UTC inside a container).
	TimeZone string `json:"timeZone"`
	// MaxMinutes bounds a window: targets not reached by then are left for
	// the next one, so updates don't spill into the working day.
	MaxMinutes int `json:"maxMinutes"`
}

var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"} // index = time.Weekday

func defaultAutoSettings() autoSettings {
	return autoSettings{Enabled: true, Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, Time: "03:00", MaxMinutes: 120}
}

// parseAutoSettings reads the saved row; empty or unreadable means defaults.
func parseAutoSettings(raw string) autoSettings {
	s := defaultAutoSettings()
	if raw == "" {
		return s
	}
	var in autoSettings
	if json.Unmarshal([]byte(raw), &in) != nil || in.validate() != nil {
		return s
	}
	return in
}

// validate checks and normalizes the settings (days in week order, no repeats).
func (s *autoSettings) validate() error {
	seen := map[string]bool{}
	for _, d := range s.Days {
		d = strings.ToLower(strings.TrimSpace(d))
		if len(d) > 3 {
			d = d[:3] // "monday" → "mon"
		}
		ok := false
		for _, w := range weekdays {
			ok = ok || w == d
		}
		if !ok {
			return fmt.Errorf("unknown day %q: use mon, tue, wed, thu, fri, sat or sun", d)
		}
		seen[d] = true
	}
	if len(seen) == 0 {
		return errors.New("choose at least one day (or turn automatic updates off)")
	}
	days := []string{}
	for _, w := range append(weekdays[1:], weekdays[0]) { // Monday first
		if seen[w] {
			days = append(days, w)
		}
	}
	s.Days = days
	s.Time = strings.TrimSpace(s.Time)
	if _, _, err := s.clock(); err != nil {
		return err
	}
	s.TimeZone = strings.TrimSpace(s.TimeZone)
	if _, err := s.location(); err != nil {
		return err
	}
	if s.MaxMinutes < 15 || s.MaxMinutes > 12*60 {
		return errors.New("the window must last between 15 minutes and 12 hours")
	}
	return nil
}

func (s autoSettings) clock() (hour, minute int, err error) {
	t, err := time.Parse("15:04", s.Time)
	if err != nil || len(s.Time) != 5 {
		return 0, 0, errors.New("the time must look like 03:00 (24-hour)")
	}
	return t.Hour(), t.Minute(), nil
}

func (s autoSettings) location() (*time.Location, error) {
	if s.TimeZone == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(s.TimeZone)
	if err != nil || s.TimeZone == "Local" {
		return nil, fmt.Errorf("unknown time zone %q: use a name like Europe/Berlin or America/New_York", s.TimeZone)
	}
	return loc, nil
}

// zoneName is the time zone the window is in, for display.
func (s autoSettings) zoneName() string {
	if s.TimeZone != "" {
		return s.TimeZone
	}
	if n := time.Local.String(); n != "Local" {
		return n
	}
	return "the hub's local time"
}

func (s autoSettings) maxDuration() time.Duration { return time.Duration(s.MaxMinutes) * time.Minute }

func (s autoSettings) allowed(d time.Weekday) bool {
	for _, x := range s.Days {
		if x == weekdays[d] {
			return true
		}
	}
	return false
}

// nextWindow is when the first window strictly after `after` starts.
//
// Each candidate day's start is built in the window's zone, minding
// daylight saving: a time skipped when clocks go forward (02:30 on that
// night) is moved past the gap (03:30), and a time that
// happens twice when they go back gives one instant, so a window never runs
// twice in a night. Since the next window is always searched strictly after
// the one that just started, the following one is a day or more later.
func nextWindow(after time.Time, s autoSettings) time.Time {
	loc, err := s.location()
	if err != nil {
		loc = time.Local
	}
	h, m, err := s.clock()
	if err != nil {
		h, m = 3, 0
	}
	local := after.In(loc)
	for i := 0; i <= 8; i++ {
		t := time.Date(local.Year(), local.Month(), local.Day()+i, h, m, 0, 0, loc)
		if lt := t.In(loc); lt.Hour() != h || lt.Minute() != m {
			// The time doesn't exist that day (clocks went forward), and Go
			// may land before the gap: move past it by the jump in offset.
			_, before := t.Zone()
			_, after := t.Add(6 * time.Hour).Zone()
			if after > before {
				t = t.Add(time.Duration(after-before) * time.Second)
			}
		}
		if t.After(after) && s.allowed(t.Weekday()) {
			return t
		}
	}
	return time.Time{} // no days chosen (validate refuses that)
}
