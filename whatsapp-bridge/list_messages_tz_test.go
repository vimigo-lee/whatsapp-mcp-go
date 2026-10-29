package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// TestListMessagesBoundsHonourOffset checks after/before against a bridge
// running in several zones (time.Local) with callers writing several offsets.
// SQLite always runs; Postgres, where the zone-less TIMESTAMP column made the
// original bug, needs a throwaway database in WA_TEST_PG_DSN.
func TestListMessagesBoundsHonourOffset(t *testing.T) {
	type backend struct {
		name, driver, dsn string
		pg                bool
	}
	backends := []backend{{"sqlite", "sqlite3", ":memory:", false}}
	if dsn := os.Getenv("WA_TEST_PG_DSN"); dsn != "" {
		backends = append(backends, backend{"postgres", "postgres", dsn, true})
	} else {
		t.Log("WA_TEST_PG_DSN not set; Postgres case skipped")
	}

	prevLocal, prevPG := time.Local, isPostgres
	defer func() { time.Local, isPostgres = prevLocal, prevPG }()

	for _, b := range backends {
		for _, zone := range []string{"UTC", "Asia/Kuala_Lumpur", "America/New_York", "Asia/Kolkata"} {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			time.Local, isPostgres = loc, b.pg
			t.Run(b.name+"/bridge-in-"+zone, func(t *testing.T) {
				checkBounds(t, b.driver, b.dsn)
			})
		}
	}
}

func checkBounds(t *testing.T, driver, dsn string) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one :memory: database for the whole check

	ph := func(n string) string {
		if isPostgres {
			return "$" + n
		}
		return "?"
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS messages`, `DROP TABLE IF EXISTS chats`,
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES ('60123456789@s.whatsapp.net', 'Ahmad', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Written the way the bridge writes them: whatsmeow hands over
	// time.Unix(...) values, i.e. in the process's local zone.
	// 01:30Z = 09:30 MYT, 02:30Z = 10:30 MYT.
	for id, sec := range map[string]int64{
		"EARLY": time.Date(2026, 10, 29, 1, 30, 0, 0, time.UTC).Unix(),
		"LATE":  time.Date(2026, 10, 29, 2, 30, 0, 0, time.UTC).Unix(),
	} {
		q := `INSERT INTO messages VALUES (` + ph("1") + `, '60123456789@s.whatsapp.net', '60123456789', ` +
			ph("2") + `, ` + ph("3") + `, false, NULL)`
		if _, err := db.Exec(q, id, id, time.Unix(sec, 0)); err != nil {
			t.Fatal(err)
		}
	}

	s := &MessageStore{db: db}
	list := func(after, before string) string {
		t.Helper()
		out, err := s.ListMessages(ListMessagesParams{After: after, Before: before, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 02:00Z sits between the two messages. Every spelling of that instant,
	// whatever offset the caller writes, must split them the same way.
	for _, bound := range []string{
		"2026-10-29T02:00:00Z",
		"2026-10-29T10:00:00+08:00", // Malaysia
		"2026-10-28T21:00:00-05:00", // New York (EST)
		"2026-10-29T07:30:00+05:30", // India
		"2026-10-29T15:00:00+13:00", // New Zealand (NZDT)
	} {
		if out := list(bound, ""); !strings.Contains(out, "LATE") || strings.Contains(out, "EARLY") {
			t.Errorf("after %s should return only LATE:\n%s", bound, out)
		}
		if out := list("", bound); !strings.Contains(out, "EARLY") || strings.Contains(out, "LATE") {
			t.Errorf("before %s should return only EARLY:\n%s", bound, out)
		}
	}
}
