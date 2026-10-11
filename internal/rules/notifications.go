package rules

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Policy applies to one notification recipient across this deployment. SnoozeUntil
// is an absolute instant; quiet hours are wall-clock times in Timezone.
type Policy struct {
	Enabled     bool
	SnoozeUntil time.Time
	QuietStart  string
	QuietEnd    string
	Timezone    string
	Revision    int64
}

func DefaultPolicy(timezone string) (Policy, error) {
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	p := Policy{Enabled: true, Timezone: timezone}
	return p, p.Validate()
}

func (p Policy) Validate() error {
	if _, err := notificationLocation(p.Timezone); err != nil {
		return err
	}
	if p.Revision < 0 {
		return errors.New("notification revision must not be negative")
	}
	if p.QuietStart == "" && p.QuietEnd == "" {
		return nil
	}
	start, err := clockMinute(p.QuietStart)
	if err != nil {
		return err
	}
	end, err := clockMinute(p.QuietEnd)
	if err != nil {
		return err
	}
	if start == end {
		return errors.New("quiet start and end must differ; use off for permanent suppression")
	}
	return nil
}

type SuppressionReason string

const (
	NotificationAllowed SuppressionReason = ""
	SuppressedOff       SuppressionReason = "off"
	SuppressedSnooze    SuppressionReason = "snooze"
	SuppressedQuiet     SuppressionReason = "quiet"
	SuppressedInvalid   SuppressionReason = "invalid"
)

// Suppression returns the first matching reason, in off/snooze/quiet order.
// Invalid policy is denied with both a nonempty reason and an error.
func (p Policy) Suppression(at time.Time) (SuppressionReason, error) {
	if err := p.Validate(); err != nil {
		return SuppressedInvalid, err
	}
	if !p.Enabled {
		return SuppressedOff, nil
	}
	if at.Before(p.SnoozeUntil) {
		return SuppressedSnooze, nil
	}
	if p.QuietStart != "" {
		loc, _ := notificationLocation(p.Timezone)
		local := at.In(loc)
		minute := local.Hour()*60 + local.Minute()
		start, _ := clockMinute(p.QuietStart)
		end, _ := clockMinute(p.QuietEnd)
		if start < end && minute >= start && minute < end || start > end && (minute >= start || minute < end) {
			return SuppressedQuiet, nil
		}
	}
	return NotificationAllowed, nil
}

type NotificationAction string

const (
	NotifyOff      NotificationAction = "off"
	NotifyOn       NotificationAction = "on"
	NotifySnooze   NotificationAction = "snooze"
	NotifyUntil    NotificationAction = "until"
	NotifyQuiet    NotificationAction = "quiet"
	NotifyQuietOff NotificationAction = "quiet_off"
	NotifyStatus   NotificationAction = "status"
)

// NotificationCommand contains validated values, with snooze/until represented
// as UTC deadlines computed at parse time rather than relative durations.
type NotificationCommand struct {
	Action      NotificationAction
	SnoozeUntil time.Time
	QuietStart  string
	QuietEnd    string
}

func ParseNotificationCommand(text string, now time.Time, timezone string) (NotificationCommand, error) {
	loc, err := notificationLocation(timezone)
	if err != nil {
		return NotificationCommand{}, err
	}
	fields := strings.Fields(text)
	if len(fields) < 3 || fields[0] != "/notify" || fields[1] != "at" {
		return NotificationCommand{}, errors.New("expected /notify at command")
	}
	c := NotificationCommand{Action: NotificationAction(fields[2])}
	switch c.Action {
	case NotifyOff, NotifyOn, NotifyStatus:
		if len(fields) != 3 {
			return NotificationCommand{}, errors.New("unexpected notification command arguments")
		}
	case NotifySnooze:
		if len(fields) != 4 {
			return NotificationCommand{}, errors.New("expected snooze duration")
		}
		duration, err := time.ParseDuration(fields[3])
		if err != nil || duration <= 0 {
			return NotificationCommand{}, errors.New("snooze duration must be a positive Go duration")
		}
		c.SnoozeUntil = now.Add(duration).UTC()
	case NotifyUntil:
		if len(fields) != 5 {
			return NotificationCommand{}, errors.New("expected until YYYY-MM-DD HH:mm")
		}
		c.SnoozeUntil, err = uniqueLocalTime(fields[3]+" "+fields[4], loc)
		if err != nil {
			return NotificationCommand{}, err
		}
		if !c.SnoozeUntil.After(now) {
			return NotificationCommand{}, errors.New("notification deadline must be in the future")
		}
	case NotifyQuiet:
		if len(fields) != 4 {
			return NotificationCommand{}, errors.New("expected quiet HH:mm-HH:mm or quiet off")
		}
		if fields[3] == "off" {
			c.Action = NotifyQuietOff
		} else {
			c.QuietStart, c.QuietEnd, _ = strings.Cut(fields[3], "-")
			if c.QuietStart == "" || c.QuietEnd == "" {
				return NotificationCommand{}, errors.New("quiet requires HH:mm-HH:mm")
			}
			p := Policy{Timezone: timezone, QuietStart: c.QuietStart, QuietEnd: c.QuietEnd}
			if err := p.Validate(); err != nil {
				return NotificationCommand{}, err
			}
		}
	default:
		return NotificationCommand{}, errors.New("unknown notification command")
	}
	return c, nil
}

// Apply is pure. Status leaves the policy and its revision unchanged; each
// successful mutation advances the revision, including repeated off/on commands.
func Apply(p Policy, c NotificationCommand, now time.Time) (Policy, error) {
	if err := p.Validate(); err != nil {
		return p, err
	}
	next := p
	switch c.Action {
	case NotifyStatus:
		return p, nil
	case NotifyOff:
		next.Enabled = false
	case NotifyOn:
		next.Enabled, next.SnoozeUntil = true, time.Time{}
	case NotifySnooze, NotifyUntil:
		if !c.SnoozeUntil.After(now) {
			return p, errors.New("notification deadline must be in the future")
		}
		next.SnoozeUntil = c.SnoozeUntil.UTC()
	case NotifyQuiet:
		if c.QuietStart == "" || c.QuietEnd == "" {
			return p, errors.New("quiet requires start and end")
		}
		next.QuietStart, next.QuietEnd = c.QuietStart, c.QuietEnd
	case NotifyQuietOff:
		next.QuietStart, next.QuietEnd = "", ""
	default:
		return p, errors.New("unknown notification command")
	}
	if p.Revision == math.MaxInt64 {
		return p, errors.New("notification revision exhausted")
	}
	next.Revision++
	if err := next.Validate(); err != nil {
		return p, err
	}
	return next, nil
}

func notificationLocation(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("notification timezone must be an IANA timezone")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid notification timezone: %w", err)
	}
	return loc, nil
}

func clockMinute(value string) (int, error) {
	t, err := time.Parse("15:04", value)
	if err != nil || t.Format("15:04") != value {
		return 0, errors.New("notification time must be HH:mm")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// uniqueLocalTime enumerates timezone intervals around the wall-clock date.
// Testing each offset by round trip detects gaps and repeated local times,
// including non-hour DST changes and historical date-line transitions.
func uniqueLocalTime(value string, loc *time.Location) (time.Time, error) {
	const layout = "2006-01-02 15:04"
	wall, err := time.Parse(layout, value)
	if err != nil || wall.Format(layout) != value {
		return time.Time{}, errors.New("notification deadline must be YYYY-MM-DD HH:mm")
	}
	offsets := map[int]bool{}
	// IANA UTC offsets fit within this window, including 24-hour date-line jumps.
	limit := wall.Add(48 * time.Hour)
	for cursor := wall.Add(-48 * time.Hour); !cursor.After(limit); {
		local := cursor.In(loc)
		_, offset := local.Zone()
		offsets[offset] = true
		_, end := local.ZoneBounds()
		if end.IsZero() || end.After(limit) {
			break
		}
		cursor = end
	}
	var result time.Time
	count := 0
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		if candidate.In(loc).Format(layout) == value {
			result = candidate.UTC()
			count++
		}
	}
	if count == 0 {
		return time.Time{}, errors.New("notification deadline is a nonexistent local time")
	}
	if count != 1 {
		return time.Time{}, errors.New("notification deadline is an ambiguous local time")
	}
	return result, nil
}
