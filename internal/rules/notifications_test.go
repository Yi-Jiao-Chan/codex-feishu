package rules

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func notificationTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestDefaultNotificationPolicyAndValidation(t *testing.T) {
	p, err := DefaultPolicy("")
	if err != nil || !p.Enabled || p.Timezone != "Asia/Shanghai" || !p.SnoozeUntil.IsZero() || p.QuietStart != "" || p.QuietEnd != "" {
		t.Fatalf("default policy: %+v %v", p, err)
	}
	if _, err := DefaultPolicy("UTC"); err != nil {
		t.Fatal(err)
	}
	if _, err := DefaultPolicy("not/a/timezone"); err == nil {
		t.Fatal("invalid default timezone accepted")
	}
	for _, invalid := range []Policy{
		{}, {Timezone: "Local"}, {Timezone: "not/a/timezone"},
		{Timezone: "UTC", Revision: -1},
		{Timezone: "UTC", QuietStart: "23:00"},
		{Timezone: "UTC", QuietEnd: "08:00"},
		{Timezone: "UTC", QuietStart: "23:00", QuietEnd: "23:00"},
		{Timezone: "UTC", QuietStart: "9:00", QuietEnd: "10:00"},
		{Timezone: "UTC", QuietStart: "23:00", QuietEnd: "24:00"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid policy accepted: %+v", invalid)
		}
		if reason, err := invalid.Suppression(time.Now()); reason != SuppressedInvalid || err == nil {
			t.Errorf("invalid policy did not fail closed: %+v %q %v", invalid, reason, err)
		}
	}
}

func TestNotificationQuietBoundaries(t *testing.T) {
	p := Policy{Enabled: true, Timezone: "Asia/Shanghai", QuietStart: "23:00", QuietEnd: "08:00"}
	for _, test := range []struct {
		at   string
		want SuppressionReason
	}{
		{"2026-10-12T22:59:59.999999999+08:00", NotificationAllowed},
		{"2026-10-12T23:00:00+08:00", SuppressedQuiet},
		{"2026-10-13T00:00:00+08:00", SuppressedQuiet},
		{"2026-10-13T07:59:00+08:00", SuppressedQuiet},
		{"2026-10-13T07:59:59.999999999+08:00", SuppressedQuiet},
		{"2026-10-13T08:00:00+08:00", NotificationAllowed},
		// Evaluation must use the policy timezone, not the input instant's zone.
		{"2026-10-12T15:00:00Z", SuppressedQuiet},
		{"2026-10-13T00:00:00Z", NotificationAllowed},
	} {
		t.Run(test.at, func(t *testing.T) {
			reason, err := p.Suppression(notificationTestTime(t, test.at))
			if err != nil || reason != test.want {
				t.Fatalf("reason = %q, want %q, err = %v", reason, test.want, err)
			}
		})
	}
	p.QuietStart, p.QuietEnd, p.Timezone = "09:00", "17:00", "UTC"
	for _, test := range []struct {
		hour int
		want SuppressionReason
	}{{8, NotificationAllowed}, {9, SuppressedQuiet}, {16, SuppressedQuiet}, {17, NotificationAllowed}} {
		reason, err := p.Suppression(time.Date(2026, 10, 12, test.hour, 0, 0, 0, time.UTC))
		if err != nil || reason != test.want {
			t.Errorf("daytime hour %d: %q %v", test.hour, reason, err)
		}
	}
}

func TestNotificationQuietTracksDST(t *testing.T) {
	p := Policy{Enabled: true, Timezone: "America/New_York", QuietStart: "01:00", QuietEnd: "02:00"}
	// Both occurrences of a repeated local hour are quiet; the rule is local daily time.
	for _, value := range []string{"2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z"} {
		if reason, err := p.Suppression(notificationTestTime(t, value)); err != nil || reason != SuppressedQuiet {
			t.Fatalf("repeated quiet hour %s: %q %v", value, reason, err)
		}
	}
	if reason, err := p.Suppression(notificationTestTime(t, "2026-11-01T07:00:00Z")); err != nil || reason != NotificationAllowed {
		t.Fatalf("quiet end after DST: %q %v", reason, err)
	}
}

func TestNotificationCombinationAndAbsoluteRestartDeadline(t *testing.T) {
	now := notificationTestTime(t, "2026-10-12T22:00:00+08:00")
	p := Policy{Enabled: false, Timezone: "Asia/Shanghai", QuietStart: "23:00", QuietEnd: "08:00"}
	c, err := ParseNotificationCommand("/notify at snooze 2h", now, p.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	p, err = Apply(p, c, now)
	if err != nil || p.Enabled || p.SnoozeUntil.Location() != time.UTC || !p.SnoozeUntil.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("snooze enabled off policy or changed deadline: %+v %v", p, err)
	}
	if reason, err := p.Suppression(now.Add(time.Hour)); err != nil || reason != SuppressedOff {
		t.Fatalf("off precedence: %q %v", reason, err)
	}
	// Serialize and reload without recomputing a relative delay.
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restarted Policy
	if err := json.Unmarshal(data, &restarted); err != nil {
		t.Fatal(err)
	}
	if !restarted.SnoozeUntil.Equal(now.Add(2 * time.Hour)) {
		t.Fatal("restart extended snooze")
	}
	restarted.Enabled = true
	if reason, err := restarted.Suppression(restarted.SnoozeUntil.Add(-time.Nanosecond)); err != nil || reason != SuppressedSnooze {
		t.Fatalf("snooze precedence: %q %v", reason, err)
	}
	if reason, err := restarted.Suppression(restarted.SnoozeUntil); err != nil || reason != SuppressedQuiet {
		t.Fatalf("deadline must expire exactly, leaving quiet: %q %v", reason, err)
	}
	restarted.QuietStart, restarted.QuietEnd = "", ""
	if reason, err := restarted.Suppression(restarted.SnoozeUntil); err != nil || reason != NotificationAllowed {
		t.Fatalf("expired snooze without quiet: %q %v", reason, err)
	}
}

func TestNotificationApplySemantics(t *testing.T) {
	now := notificationTestTime(t, "2026-10-12T10:00:00Z")
	original := Policy{Enabled: false, Timezone: "UTC", SnoozeUntil: now.Add(time.Hour), QuietStart: "23:00", QuietEnd: "08:00", Revision: 7}
	enabled := original
	enabled.Enabled = true
	longTermOff, err := Apply(enabled, NotificationCommand{Action: NotifyOff}, now)
	if err != nil || longTermOff.Enabled || !longTermOff.SnoozeUntil.Equal(enabled.SnoozeUntil) || longTermOff.QuietStart != enabled.QuietStart || longTermOff.QuietEnd != enabled.QuietEnd {
		t.Fatalf("off altered snooze/quiet: %+v %v", longTermOff, err)
	}
	on, err := Apply(original, NotificationCommand{Action: NotifyOn}, now)
	if err != nil || !on.Enabled || !on.SnoozeUntil.IsZero() || on.QuietStart != original.QuietStart || on.QuietEnd != original.QuietEnd || on.Revision != 8 {
		t.Fatalf("on: %+v %v", on, err)
	}
	off, err := Apply(on, NotificationCommand{Action: NotifyOff}, now)
	if err != nil || off.Enabled || off.QuietStart != on.QuietStart || off.QuietEnd != on.QuietEnd {
		t.Fatalf("off: %+v %v", off, err)
	}
	quietOff, err := Apply(original, NotificationCommand{Action: NotifyQuietOff}, now)
	if err != nil || quietOff.Enabled || !quietOff.SnoozeUntil.Equal(original.SnoozeUntil) || quietOff.QuietStart != "" || quietOff.QuietEnd != "" {
		t.Fatalf("quiet off altered off/snooze: %+v %v", quietOff, err)
	}
	quiet, err := Apply(original, NotificationCommand{Action: NotifyQuiet, QuietStart: "09:00", QuietEnd: "10:00"}, now)
	if err != nil || quiet.Enabled || !quiet.SnoozeUntil.Equal(original.SnoozeUntil) || quiet.QuietStart != "09:00" || quiet.QuietEnd != "10:00" || quiet.Timezone != original.Timezone {
		t.Fatalf("quiet: %+v %v", quiet, err)
	}
	status, err := Apply(original, NotificationCommand{Action: NotifyStatus}, now)
	if err != nil || !reflect.DeepEqual(status, original) {
		t.Fatalf("status mutated policy: %+v %v", status, err)
	}
	for _, c := range []NotificationCommand{
		{Action: "unknown"}, {Action: NotifyQuiet},
		{Action: NotifyQuiet, QuietStart: "23:00", QuietEnd: "23:00"},
		{Action: NotifyQuiet, QuietStart: "23:00", QuietEnd: "24:00"},
		{Action: NotifySnooze}, {Action: NotifyUntil, SnoozeUntil: now},
	} {
		unchanged, err := Apply(original, c, now)
		if err == nil || !reflect.DeepEqual(unchanged, original) {
			t.Errorf("bad command altered policy: %+v => %+v %v", c, unchanged, err)
		}
	}
	invalid := Policy{Timezone: "invalid"}
	if _, err := Apply(invalid, NotificationCommand{Action: NotifyOn}, now); err == nil {
		t.Fatal("invalid existing policy accepted")
	}
	original.Revision = math.MaxInt64
	if _, err := Apply(original, NotificationCommand{Action: NotifyOff}, now); err == nil {
		t.Fatal("revision overflow accepted")
	}
}

func TestParseNotificationCommands(t *testing.T) {
	now := notificationTestTime(t, "2026-10-12T00:00:00Z")
	for _, test := range []struct {
		text string
		want NotificationCommand
	}{
		{"/notify at off", NotificationCommand{Action: NotifyOff}},
		{"/notify at on", NotificationCommand{Action: NotifyOn}},
		{"/notify at status", NotificationCommand{Action: NotifyStatus}},
		{"/notify at quiet off", NotificationCommand{Action: NotifyQuietOff}},
		{"/notify at quiet 23:00-08:00", NotificationCommand{Action: NotifyQuiet, QuietStart: "23:00", QuietEnd: "08:00"}},
		{"/notify at snooze 2h", NotificationCommand{Action: NotifySnooze, SnoozeUntil: now.Add(2 * time.Hour)}},
		{"/notify at snooze 1h30m", NotificationCommand{Action: NotifySnooze, SnoozeUntil: now.Add(90 * time.Minute)}},
		{"/notify at until 2026-10-13 09:00", NotificationCommand{Action: NotifyUntil, SnoozeUntil: notificationTestTime(t, "2026-10-13T01:00:00Z")}},
	} {
		t.Run(test.text, func(t *testing.T) {
			got, err := ParseNotificationCommand(test.text, now, "Asia/Shanghai")
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, want %+v, err %v", got, test.want, err)
			}
		})
	}
}

func TestParseNotificationRejectsInvalidCommands(t *testing.T) {
	now := notificationTestTime(t, "2026-10-12T00:00:00Z")
	for _, text := range []string{
		"", "/notify", "/notify at", "/notify off", "/notify all off", "notify at off", "/notify at unknown",
		"/notify at quiet_off", "/notify at on extra", "/notify at off extra", "/notify at status extra",
		"/notify at snooze", "/notify at snooze 2h extra", "/notify at snooze 0h", "/notify at snooze -1h", "/notify at snooze 1d", "/notify at snooze forever", "/notify at snooze 1000000000000h",
		"/notify at until", "/notify at until 2026-10-13", "/notify at until 2026-10-13 09:00 extra", "/notify at until 2026-10-12 00:00", "/notify at until 2026-10-11 23:59",
		"/notify at until 2026-02-30 09:00", "/notify at until 2026-10-13 24:00", "/notify at until 2026-10-13 9:00", "/notify at until 2026-10-13 09:00:00",
		"/notify at quiet", "/notify at quiet off extra", "/notify at quiet 23:00-08:00 extra", "/notify at quiet 23:00-23:00", "/notify at quiet 24:00-08:00", "/notify at quiet 23:00-8:00",
		"/notify at quiet 23:00", "/notify at quiet -", "/notify at quiet -08:00", "/notify at quiet 23:00-", "/notify at quiet 23:00-08:00-09:00",
	} {
		t.Run(text, func(t *testing.T) {
			if got, err := ParseNotificationCommand(text, now, "UTC"); err == nil {
				t.Fatalf("invalid command accepted: %+v", got)
			}
		})
	}
	for _, timezone := range []string{"", "Local", "not/a/timezone", "+08:00"} {
		if _, err := ParseNotificationCommand("/notify at off", now, timezone); err == nil {
			t.Errorf("invalid timezone accepted: %q", timezone)
		}
	}
}

func TestNotificationUntilDSTGapAndAmbiguity(t *testing.T) {
	now := notificationTestTime(t, "2010-01-01T00:00:00Z")
	for _, test := range []struct {
		timezone, wall, reason string
	}{
		{"America/New_York", "2026-03-08 02:30", "nonexistent"},
		{"America/New_York", "2026-11-01 01:30", "ambiguous"},
		{"Australia/Lord_Howe", "2026-10-04 02:15", "nonexistent"},
		{"Australia/Lord_Howe", "2026-04-05 01:45", "ambiguous"},
		{"Pacific/Apia", "2011-12-30 09:00", "nonexistent"},
	} {
		t.Run(test.timezone+test.wall, func(t *testing.T) {
			_, err := ParseNotificationCommand("/notify at until "+test.wall, now, test.timezone)
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("wanted %s, got %v", test.reason, err)
			}
		})
	}
	for _, test := range []struct {
		wall, utc string
	}{
		{"2026-03-08 01:59", "2026-03-08T06:59:00Z"},
		{"2026-03-08 03:00", "2026-03-08T07:00:00Z"},
		{"2026-11-01 02:00", "2026-11-01T07:00:00Z"},
	} {
		got, err := ParseNotificationCommand("/notify at until "+test.wall, now, "America/New_York")
		if err != nil || !got.SnoozeUntil.Equal(notificationTestTime(t, test.utc)) || got.SnoozeUntil.Location() != time.UTC {
			t.Errorf("unambiguous %s: %+v %v", test.wall, got, err)
		}
	}
}
