package rules

import (
	"testing"
	"time"
)

func TestNotificationSuppressionBetweenQuiet(t *testing.T) {
	for _, test := range []struct {
		name, timezone, start, end, from, to string
		want                                 SuppressionReason
	}{
		{"UTC whole overnight", "UTC", "23:00", "08:00", "2026-10-12T22:00:00Z", "2026-10-13T09:00:00Z", SuppressedQuiet},
		{"Shanghai whole overnight", "Asia/Shanghai", "23:00", "08:00", "2026-10-12T14:00:00Z", "2026-10-13T01:00:00Z", SuppressedQuiet},
		{"Shanghai daytime allowed", "Asia/Shanghai", "23:00", "08:00", "2026-10-13T00:00:00Z", "2026-10-13T14:59:59.999999999Z", NotificationAllowed},
		{"before daytime start", "UTC", "09:00", "17:00", "2026-10-12T08:00:00Z", "2026-10-12T08:59:59.999999999Z", NotificationAllowed},
		{"start at final endpoint", "UTC", "09:00", "17:00", "2026-10-12T08:00:00Z", "2026-10-12T09:00:00Z", SuppressedQuiet},
		{"quiet end is excluded", "UTC", "09:00", "17:00", "2026-10-12T17:00:00Z", "2026-10-12T17:00:00.000000001Z", NotificationAllowed},
		{"last quiet nanosecond", "UTC", "09:00", "17:00", "2026-10-12T16:59:59.999999999Z", "2026-10-12T17:00:00Z", SuppressedQuiet},
		{"single quiet instant", "UTC", "23:00", "08:00", "2026-10-12T23:00:00Z", "2026-10-12T23:00:00Z", SuppressedQuiet},
		{"single allowed instant", "UTC", "23:00", "08:00", "2026-10-13T08:00:00Z", "2026-10-13T08:00:00Z", NotificationAllowed},
		{"UTC date boundary", "UTC", "00:00", "00:01", "2026-10-12T23:59:59.999999999Z", "2026-10-13T00:00:00Z", SuppressedQuiet},
		{"historical offset seconds", "Asia/Shanghai", "09:00", "09:01", "1800-01-01T00:54:16.999999999Z", "1800-01-01T00:54:17Z", SuppressedQuiet},
		{"historical offset before boundary", "Asia/Shanghai", "09:00", "09:01", "1800-01-01T00:54:16Z", "1800-01-01T00:54:16.999999999Z", NotificationAllowed},
		{"UTC centuries", "UTC", "09:00", "09:01", "1200-01-01T10:00:00Z", "2800-12-31T08:00:00Z", SuppressedQuiet},
		{"Shanghai centuries", "Asia/Shanghai", "09:00", "10:00", "1600-01-01T12:00:00Z", "2600-12-31T12:00:00Z", SuppressedQuiet},
		{"future leap year quiet", "America/New_York", "01:00", "02:00", "2040-12-31T03:00:00Z", "2040-12-31T06:00:00Z", SuppressedQuiet},
		{"future leap year allowed", "America/New_York", "01:00", "02:00", "2040-12-31T03:00:00Z", "2040-12-31T05:59:59.999999999Z", NotificationAllowed},
		{"future leap year into next year", "America/New_York", "23:00", "00:00", "2040-12-31T22:00:00Z", "2041-01-01T04:00:00Z", SuppressedQuiet},
		{"future southern leap year", "Australia/Lord_Howe", "11:30", "12:00", "2040-12-31T00:00:00Z", "2040-12-31T00:30:00Z", SuppressedQuiet},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := Policy{Enabled: true, Timezone: test.timezone, QuietStart: test.start, QuietEnd: test.end}
			reason, err := p.SuppressionBetween(notificationTestTime(t, test.from), notificationTestTime(t, test.to))
			if err != nil || reason != test.want {
				t.Fatalf("reason = %q, want %q, err = %v", reason, test.want, err)
			}
		})
	}
}

func TestNotificationSuppressionBetweenTimezoneTransitions(t *testing.T) {
	for _, test := range []struct {
		name, timezone, start, end, from, to string
		want                                 SuppressionReason
	}{
		{"New York entire spring gap", "America/New_York", "02:00", "03:00", "2026-03-08T06:00:00Z", "2026-03-08T08:00:00Z", NotificationAllowed},
		{"New York inside spring gap", "America/New_York", "02:15", "02:45", "2026-03-08T06:59:59.999999999Z", "2026-03-08T07:00:00Z", NotificationAllowed},
		{"New York spring transition endpoint", "America/New_York", "02:30", "03:15", "2026-03-08T06:59:59.999999999Z", "2026-03-08T07:00:00Z", SuppressedQuiet},
		{"New York before spring transition", "America/New_York", "03:00", "03:01", "2026-03-08T06:59:59Z", "2026-03-08T06:59:59.999999999Z", NotificationAllowed},
		{"New York spring quiet end", "America/New_York", "02:30", "03:15", "2026-03-08T07:15:00Z", "2026-03-08T08:00:00Z", NotificationAllowed},
		{"New York pre-gap quiet", "America/New_York", "01:59", "02:00", "2026-03-08T06:58:59Z", "2026-03-08T07:00:00Z", SuppressedQuiet},
		{"New York first repeated quiet", "America/New_York", "01:15", "01:30", "2026-11-01T05:20:00Z", "2026-11-01T05:20:00Z", SuppressedQuiet},
		{"New York second repeated quiet", "America/New_York", "01:15", "01:30", "2026-11-01T06:20:00Z", "2026-11-01T06:20:00Z", SuppressedQuiet},
		{"New York gap between repeated quiet", "America/New_York", "01:15", "01:30", "2026-11-01T05:30:00Z", "2026-11-01T06:14:59.999999999Z", NotificationAllowed},
		{"New York second repeated start", "America/New_York", "01:15", "01:30", "2026-11-01T05:30:00Z", "2026-11-01T06:15:00Z", SuppressedQuiet},
		{"New York second repeated end", "America/New_York", "01:15", "01:30", "2026-11-01T06:30:00Z", "2026-11-01T06:30:00Z", NotificationAllowed},
		{"New York fold instant", "America/New_York", "01:00", "01:15", "2026-11-01T05:15:00Z", "2026-11-01T06:00:00Z", SuppressedQuiet},
		{"Lord Howe spring gap", "Australia/Lord_Howe", "02:05", "02:20", "2026-10-03T15:29:00Z", "2026-10-03T15:31:00Z", NotificationAllowed},
		{"Lord Howe spring transition endpoint", "Australia/Lord_Howe", "02:15", "02:45", "2026-10-03T15:29:59.999999999Z", "2026-10-03T15:30:00Z", SuppressedQuiet},
		{"Lord Howe spring excluded end", "Australia/Lord_Howe", "02:00", "02:30", "2026-10-03T15:29:59.999999999Z", "2026-10-03T15:30:00Z", NotificationAllowed},
		{"Lord Howe first repeated quiet", "Australia/Lord_Howe", "01:45", "01:50", "2026-04-04T14:45:00Z", "2026-04-04T14:45:00Z", SuppressedQuiet},
		{"Lord Howe second repeated quiet", "Australia/Lord_Howe", "01:45", "01:50", "2026-04-04T15:15:00Z", "2026-04-04T15:15:00Z", SuppressedQuiet},
		{"Lord Howe between repeated quiet", "Australia/Lord_Howe", "01:45", "01:50", "2026-04-04T14:50:00Z", "2026-04-04T15:14:59.999999999Z", NotificationAllowed},
		{"Lord Howe second start endpoint", "Australia/Lord_Howe", "01:45", "01:50", "2026-04-04T14:50:00Z", "2026-04-04T15:15:00Z", SuppressedQuiet},
		{"Apia missing date has no quiet", "Pacific/Apia", "09:00", "10:00", "2011-12-29T20:00:00Z", "2011-12-30T18:59:59.999999999Z", NotificationAllowed},
		{"Apia quiet after missing date", "Pacific/Apia", "09:00", "10:00", "2011-12-29T20:00:00Z", "2011-12-30T19:00:00Z", SuppressedQuiet},
		{"Apia missing date transition", "Pacific/Apia", "00:00", "00:01", "2011-12-30T09:59:59.999999999Z", "2011-12-30T10:00:00Z", SuppressedQuiet},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := Policy{Enabled: true, Timezone: test.timezone, QuietStart: test.start, QuietEnd: test.end}
			reason, err := p.SuppressionBetween(notificationTestTime(t, test.from), notificationTestTime(t, test.to))
			if err != nil || reason != test.want {
				t.Fatalf("reason = %q, want %q, err = %v", reason, test.want, err)
			}
		})
	}
}

func TestNotificationSuppressionBetweenPrecedence(t *testing.T) {
	from := notificationTestTime(t, "2026-10-12T09:00:00Z")
	to := notificationTestTime(t, "2026-10-12T15:00:00Z")
	deadline := from.Add(time.Hour)
	for _, test := range []struct {
		name string
		p    Policy
		from time.Time
		to   time.Time
		want SuppressionReason
	}{
		{"off precedes snooze and quiet", Policy{Timezone: "UTC", SnoozeUntil: deadline, QuietStart: "12:00", QuietEnd: "13:00"}, from, to, SuppressedOff},
		{"snooze covers only beginning", Policy{Enabled: true, Timezone: "UTC", SnoozeUntil: deadline, QuietStart: "12:00", QuietEnd: "13:00"}, from, to, SuppressedSnooze},
		{"last snoozed nanosecond", Policy{Enabled: true, Timezone: "UTC", SnoozeUntil: deadline}, deadline.Add(-time.Nanosecond), deadline, SuppressedSnooze},
		{"snooze expires exactly", Policy{Enabled: true, Timezone: "UTC", SnoozeUntil: deadline}, deadline, deadline, NotificationAllowed},
		{"expired snooze still quiet later", Policy{Enabled: true, Timezone: "UTC", SnoozeUntil: from, QuietStart: "12:00", QuietEnd: "13:00"}, from, to, SuppressedQuiet},
		{"expired snooze allowed", Policy{Enabled: true, Timezone: "UTC", SnoozeUntil: from.Add(-time.Hour)}, from, to, NotificationAllowed},
		{"no rules across centuries", Policy{Enabled: true, Timezone: "UTC"}, notificationTestTime(t, "1200-01-01T00:00:00Z"), notificationTestTime(t, "2800-12-31T23:59:59Z"), NotificationAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			reason, err := test.p.SuppressionBetween(test.from, test.to)
			if err != nil || reason != test.want {
				t.Fatalf("reason = %q, want %q, err = %v", reason, test.want, err)
			}
		})
	}
}

func TestNotificationSuppressionBetweenInvalid(t *testing.T) {
	at := notificationTestTime(t, "2026-10-12T09:00:00Z")
	for _, invalid := range []Policy{
		{}, {Timezone: "Local"}, {Timezone: "not/a/timezone"},
		{Timezone: "UTC", Revision: -1},
		{Timezone: "UTC", QuietStart: "23:00"},
		{Timezone: "UTC", QuietEnd: "08:00"},
		{Timezone: "UTC", QuietStart: "23:00", QuietEnd: "23:00"},
		{Timezone: "UTC", QuietStart: "9:00", QuietEnd: "10:00"},
		{Timezone: "UTC", QuietStart: "23:00", QuietEnd: "24:00"},
	} {
		if reason, err := invalid.SuppressionBetween(at, at.Add(time.Hour)); reason != SuppressedInvalid || err == nil {
			t.Errorf("invalid policy did not fail closed: %+v %q %v", invalid, reason, err)
		}
	}
	for _, p := range []Policy{{Enabled: true, Timezone: "UTC"}, {Timezone: "UTC", SnoozeUntil: at.Add(time.Hour)}} {
		if reason, err := p.SuppressionBetween(at, at.Add(-time.Nanosecond)); reason != SuppressedInvalid || err == nil {
			t.Errorf("reversed interval did not fail closed: %+v %q %v", p, reason, err)
		}
	}
}

// These fixtures have minute-aligned UTC offsets and transitions. At this
// resolution, any nonempty quiet intersection contains a sampled instant.
// The oracle uses the existing point policy, independently of ZoneBounds.
func TestNotificationSuppressionBetweenMatchesPointPolicy(t *testing.T) {
	for _, window := range []struct {
		timezone, from string
	}{
		{"America/New_York", "2026-03-08T04:00:00Z"},
		{"America/New_York", "2026-11-01T04:00:00Z"},
		{"Australia/Lord_Howe", "2026-10-03T13:00:00Z"},
		{"Australia/Lord_Howe", "2026-04-04T13:00:00Z"},
		{"Pacific/Apia", "2011-12-30T08:00:00Z"},
	} {
		from := notificationTestTime(t, window.from)
		for _, quiet := range [][2]string{{"00:00", "00:01"}, {"01:15", "01:30"}, {"02:00", "03:00"}, {"02:15", "02:45"}, {"23:59", "00:01"}} {
			p := Policy{Enabled: true, Timezone: window.timezone, QuietStart: quiet[0], QuietEnd: quiet[1]}
			var counts [362]int
			for minute := 0; minute <= 360; minute++ {
				reason, err := p.Suppression(from.Add(time.Duration(minute) * time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				counts[minute+1] = counts[minute]
				if reason == SuppressedQuiet {
					counts[minute+1]++
				}
			}
			bounds := []int{0, 59, 120, 179, 240, 360}
			for i, first := range bounds {
				for _, last := range bounds[i:] {
					want := NotificationAllowed
					if counts[last+1] != counts[first] {
						want = SuppressedQuiet
					}
					reason, err := p.SuppressionBetween(from.Add(time.Duration(first)*time.Minute), from.Add(time.Duration(last)*time.Minute))
					if err != nil || reason != want {
						t.Fatalf("%s %s quiet %s-%s minutes [%d,%d]: %q, want %q, err %v", window.timezone, window.from, quiet[0], quiet[1], first, last, reason, want, err)
					}
				}
			}
		}
	}
}
