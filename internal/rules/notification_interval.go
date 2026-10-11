package rules

import (
	"errors"
	"time"
)

// SuppressionBetween reports whether any instant in the closed interval is
// suppressed by this fixed policy, in off/snooze/quiet order. Invalid policies
// and reversed intervals fail closed, as Suppression does for invalid policies.
func (p Policy) SuppressionBetween(fromInclusive, toInclusive time.Time) (SuppressionReason, error) {
	if err := p.Validate(); err != nil {
		return SuppressedInvalid, err
	}
	if toInclusive.Before(fromInclusive) {
		return SuppressedInvalid, errors.New("notification interval end precedes start")
	}
	if !p.Enabled {
		return SuppressedOff, nil
	}
	if fromInclusive.Before(p.SnoozeUntil) {
		return SuppressedSnooze, nil
	}
	if p.QuietStart == "" {
		return NotificationAllowed, nil
	}

	loc, _ := notificationLocation(p.Timezone)
	start, _ := clockMinute(p.QuietStart)
	end, _ := clockMinute(p.QuietEnd)
	for cursor := fromInclusive.In(loc); ; {
		minute := cursor.Hour()*60 + cursor.Minute()
		if start < end && minute >= start && minute < end || start > end && (minute >= start || minute < end) {
			return SuppressedQuiet, nil
		}

		// Within a constant-offset segment, wall time advances with real time.
		// The next daily start is at most 24 hours away, even if the queried
		// interval spans centuries. Do not construct a local Date: that would
		// choose one occurrence in a fold or invent a time inside a gap.
		untilStart := time.Duration(start)*time.Minute -
			time.Duration(cursor.Hour())*time.Hour -
			time.Duration(cursor.Minute())*time.Minute -
			time.Duration(cursor.Second())*time.Second -
			time.Duration(cursor.Nanosecond())
		if untilStart <= 0 {
			untilStart += 24 * time.Hour
		}
		nextStart := cursor.Add(untilStart)
		_, zoneEnd := cursor.ZoneBounds()
		if !zoneEnd.IsZero() && !zoneEnd.After(cursor) {
			// Go's POSIX timezone extension can end the final segment at
			// year-start + 365 days, including in leap years. On December 31
			// this is a stale bound; that final offset lasts through January 1.
			utc := cursor.UTC()
			nextYear := time.Date(utc.Year()+1, time.January, 1, 0, 0, 0, 0, time.UTC)
			if utc.YearDay() != 366 || !zoneEnd.Equal(nextYear.Add(-24*time.Hour)) {
				return SuppressedInvalid, errors.New("notification timezone interval does not advance")
			}
			zoneEnd = nextYear.In(loc)
		}
		if !nextStart.After(toInclusive) && (zoneEnd.IsZero() || nextStart.Before(zoneEnd)) {
			return SuppressedQuiet, nil
		}
		if zoneEnd.IsZero() || zoneEnd.After(toInclusive) {
			return NotificationAllowed, nil
		}
		// The old offset's endpoint is excluded, but the new offset's first
		// instant is included, including when it is also toInclusive.
		cursor = zoneEnd
	}
}
