package main

import (
	"testing"
	"time"
)

func TestParseReminderSchedule(t *testing.T) {
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)

	// in_minutes
	at, m, err := parseReminderSchedule(reminderSchedule{InMinutes: 15}, now)
	if err != nil || m["recur"] != "once" || at != now.Add(15*time.Minute).Unix() {
		t.Fatalf("in_minutes: at=%d m=%v err=%v", at, m, err)
	}

	// every_minutes
	at, m, err = parseReminderSchedule(reminderSchedule{EveryMinutes: 30}, now)
	if err != nil || m["recur"] != "every" || m["interval_min"] != "30" || at != now.Add(30*time.Minute).Unix() {
		t.Fatalf("every_minutes: at=%d m=%v err=%v", at, m, err)
	}

	// at (future)
	at, m, err = parseReminderSchedule(reminderSchedule{At: "2026-06-25T18:00:00+03:00"}, now)
	if err != nil || m["recur"] != "once" {
		t.Fatalf("at future: m=%v err=%v", m, err)
	}
	if want := time.Date(2026, 6, 25, 15, 0, 0, 0, time.UTC).Unix(); at != want {
		t.Fatalf("at: got %d want %d", at, want)
	}

	// at (past) -> error
	if _, _, err := parseReminderSchedule(reminderSchedule{At: "2020-01-01T00:00:00Z"}, now); err == nil {
		t.Fatal("past 'at' must error")
	}

	// daily_at requires tz
	if _, _, err := parseReminderSchedule(reminderSchedule{DailyAt: "09:00"}, now); err == nil {
		t.Fatal("daily_at without tz must error")
	}

	// daily_at with tz, time later today
	at, m, err = parseReminderSchedule(reminderSchedule{DailyAt: "23:30", TZ: "Europe/Kyiv"}, now)
	if err != nil || m["recur"] != "daily" || m["hhmm"] != "23:30" || m["tz"] != "Europe/Kyiv" {
		t.Fatalf("daily_at: m=%v err=%v", m, err)
	}
	loc, _ := time.LoadLocation("Europe/Kyiv")
	if got := time.Unix(at, 0).In(loc); got.Hour() != 23 || got.Minute() != 30 {
		t.Fatalf("daily next fire wrong clock: %v", got)
	}

	// no field set -> error
	if _, _, err := parseReminderSchedule(reminderSchedule{}, now); err == nil {
		t.Fatal("empty schedule must error")
	}
}

func TestNextDailyRollsToTomorrow(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Kyiv")
	// now is 12:00 Kyiv; a 09:00 reminder must be tomorrow.
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, loc)
	next := time.Unix(nextDaily(now, 9, 0, loc), 0).In(loc)
	if next.Day() != 26 || next.Hour() != 9 {
		t.Fatalf("expected tomorrow 09:00, got %v", next)
	}
	// a 15:00 reminder is later today.
	next = time.Unix(nextDaily(now, 15, 0, loc), 0).In(loc)
	if next.Day() != 25 || next.Hour() != 15 {
		t.Fatalf("expected today 15:00, got %v", next)
	}
}
