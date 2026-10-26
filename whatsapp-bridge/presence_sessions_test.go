package main

import (
	"testing"
	"time"
)

func TestPresenceSessionDue(t *testing.T) {
	loc := time.FixedZone("MYT", 8*3600)
	at := func(h int) time.Time { return time.Date(2026, 10, 26, h, 30, 0, 0, loc) }
	idle := 36 * time.Hour
	cases := []struct {
		name string
		now  time.Time
		last time.Time
		want bool
	}{
		{"daytime, idle long enough", at(10), at(10).Add(-40 * time.Hour), true},
		{"daytime, recently active", at(10), at(10).Add(-2 * time.Hour), false},
		{"before day start", at(8), at(8).Add(-40 * time.Hour), false},
		{"last daytime hour", at(20), at(20).Add(-40 * time.Hour), true},
		{"after day end", at(21), at(21).Add(-40 * time.Hour), false},
		{"never active, daytime", at(12), time.Time{}, true},
	}
	for _, c := range cases {
		if got := presenceSessionDue(c.now, c.last, idle, loc); got != c.want {
			t.Errorf("%s: presenceSessionDue = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRandDurationBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := randDuration(presenceLingerMin, presenceLingerMax)
		if d < presenceLingerMin || d >= presenceLingerMax {
			t.Fatalf("randDuration = %v, outside [%v, %v)", d, presenceLingerMin, presenceLingerMax)
		}
	}
	if d := randDuration(time.Minute, time.Minute); d != time.Minute {
		t.Fatalf("randDuration(min == max) = %v, want %v", d, time.Minute)
	}
}

func TestPresenceReadThrottle(t *testing.T) {
	p := &presenceSessions{}

	p.read(nil)
	first := p.lastRead
	if first.IsZero() {
		t.Fatal("first read should open a session")
	}

	p.read(nil)
	if !p.lastRead.Equal(first) {
		t.Fatal("a second read within the hour must not open another session")
	}

	hourAgo := time.Now().Add(-presenceReadEvery)
	p.lastRead = hourAgo
	p.online = true
	p.read(nil)
	if !p.lastRead.Equal(hourAgo) {
		t.Fatal("a read while already online must not open a session")
	}

	p.online = false
	p.read(nil)
	if time.Since(p.lastRead) > time.Minute {
		t.Fatal("a read after the hour should open a new session")
	}
}
