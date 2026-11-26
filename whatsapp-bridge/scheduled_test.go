package main

import (
	"bytes"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// A sealed message opens with its key into the original, with a message
// secret added, and the outer message has WA Web's shape.
func TestSealScheduledMessageRoundTrip(t *testing.T) {
	inner := &waE2E.Message{Conversation: proto.String("see you at 3")}
	s, err := sealScheduledMessage(inner)
	if err != nil {
		t.Fatal(err)
	}
	if inner.MessageContextInfo != nil {
		t.Error("caller's message was modified")
	}
	if len(s.RevealKey) != revealKeyLen {
		t.Fatalf("key is %d bytes", len(s.RevealKey))
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(s.KeyID) {
		t.Errorf("key id %q is not a v4 UUID", s.KeyID)
	}

	cr := s.Outer.GetConditionalRevealMessage()
	if cr == nil {
		t.Fatal("outer has no ConditionalRevealMessage")
	}
	if cr.GetConditionalRevealMessageType() != waE2E.ConditionalRevealMessage_SCHEDULED_MESSAGE {
		t.Errorf("type = %v", cr.GetConditionalRevealMessageType())
	}
	if cr.GetRevealKeyID() != s.KeyID {
		t.Errorf("revealKeyID = %q, want %q", cr.GetRevealKeyID(), s.KeyID)
	}
	if len(cr.GetEncIV()) != revealIVLen {
		t.Errorf("iv is %d bytes", len(cr.GetEncIV()))
	}
	if s.Outer.Conversation != nil || s.Outer.ExtendedTextMessage != nil {
		t.Error("plaintext leaked into the outer message")
	}
	if bytes.Contains(cr.GetEncPayload(), []byte("see you at 3")) {
		t.Error("payload is not encrypted")
	}

	got, err := openScheduledMessage(cr, s.RevealKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetConversation() != "see you at 3" {
		t.Errorf("revealed %q", got.GetConversation())
	}
	secret := got.GetMessageContextInfo().GetMessageSecret()
	if len(secret) != 32 {
		t.Fatalf("inner secret is %d bytes", len(secret))
	}
	if !bytes.Equal(secret, s.Outer.GetMessageContextInfo().GetMessageSecret()) {
		t.Error("outer and inner message secrets differ")
	}

	// The wrong key, or a flipped byte, must not open it.
	other := bytes.Repeat([]byte{7}, revealKeyLen)
	if _, err := openScheduledMessage(cr, other); err == nil {
		t.Error("opened with the wrong key")
	}
	bad := proto.Clone(cr).(*waE2E.ConditionalRevealMessage)
	bad.EncPayload[0] ^= 1
	if _, err := openScheduledMessage(bad, s.RevealKey); err == nil {
		t.Error("opened a tampered payload")
	}
}

// An existing message secret is kept.
func TestSealScheduledMessageKeepsSecret(t *testing.T) {
	secret := bytes.Repeat([]byte{9}, 32)
	inner := &waE2E.Message{
		Conversation:       proto.String("x"),
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}
	s, err := sealScheduledMessage(inner)
	if err != nil {
		t.Fatal(err)
	}
	got, err := openScheduledMessage(s.Outer.GetConditionalRevealMessage(), s.RevealKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.GetMessageContextInfo().GetMessageSecret(), secret) ||
		!bytes.Equal(s.Outer.GetMessageContextInfo().GetMessageSecret(), secret) {
		t.Error("message secret was replaced")
	}
}

// Known-answer check: what WA Web's WebCrypto AES-GCM (tag appended) makes
// for a fixed key and IV is what Go's GCM Seal makes, so this pins the
// format: no AAD, 16-byte tag at the end.
func TestRevealSealFormat(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	iv := bytes.Repeat([]byte{2}, 12)
	enc, err := revealSeal(key, iv, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != 2+16 {
		t.Fatalf("ciphertext is %d bytes, want plaintext+16", len(enc))
	}
	plain, err := revealOpen(key, iv, enc)
	if err != nil || string(plain) != "hi" {
		t.Fatalf("open: %q %v", plain, err)
	}
	if _, err := revealSeal(key[:16], iv, nil); err == nil {
		t.Error("accepted a 16-byte key")
	}
	if _, err := revealSeal(key, iv[:8], nil); err == nil {
		t.Error("accepted an 8-byte iv")
	}
}

func TestScheduleMetaNode(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	at := time.Unix(1764150000, 0)
	n := scheduleMetaNode("abc", key, at)
	if n.Tag != "meta" || n.Attrs["type"] != "scheduled_message" || n.Attrs["st"] != "1764150000" {
		t.Fatalf("meta = %v", n)
	}
	kids, ok := n.Content.([]waBinary.Node)
	if !ok || len(kids) != 1 {
		t.Fatalf("meta children = %#v", n.Content)
	}
	k := kids[0]
	if k.Tag != "key" || k.Attrs["rkid"] != "abc" || !bytes.Equal(k.Content.([]byte), key) {
		t.Errorf("key node = %v", k)
	}

	u := unscheduleMetaNode("abc")
	if u.Attrs["type"] != "scheduled_message" {
		t.Errorf("unschedule meta = %v", u)
	}
	if _, has := u.Attrs["st"]; has {
		t.Error("unschedule meta has st")
	}
	uk := u.Content.([]waBinary.Node)[0]
	if uk.Attrs["rkid"] != "abc" || uk.Content != nil {
		t.Errorf("unschedule key node = %v", uk)
	}
}

func TestBuildUnscheduleMessage(t *testing.T) {
	me := types.NewJID("60111", types.DefaultUserServer)
	dm := types.NewJID("60222", types.DefaultUserServer)
	m := buildUnscheduleMessage(dm, "3EB0AA", me)
	pm := m.GetProtocolMessage()
	if pm.GetType() != waE2E.ProtocolMessage_MESSAGE_UNSCHEDULE {
		t.Errorf("type = %v", pm.GetType())
	}
	k := pm.GetKey()
	if k.GetRemoteJID() != dm.String() || !k.GetFromMe() || k.GetID() != "3EB0AA" || k.Participant != nil {
		t.Errorf("dm key = %v", k)
	}

	group := types.NewJID("1203", types.GroupServer)
	g := buildUnscheduleMessage(group, "3EB0BB", types.NewADJID("60111", 0, 5))
	if g.GetProtocolMessage().GetKey().GetParticipant() != "60111@s.whatsapp.net" {
		t.Errorf("group participant = %q", g.GetProtocolMessage().GetKey().GetParticipant())
	}
}

func TestNormalizeScheduleTime(t *testing.T) {
	now := time.Date(2026, 11, 26, 15, 21, 40, 0, time.UTC)
	cases := []struct {
		at   time.Time
		ok   bool
		want time.Time
	}{
		// 10 minutes past the start of the current minute is the earliest,
		// as WhatsApp's own picker allows (15:21 -> 15:31).
		{time.Date(2026, 11, 26, 15, 31, 0, 0, time.UTC), true, time.Date(2026, 11, 26, 15, 31, 0, 0, time.UTC)},
		{time.Date(2026, 11, 26, 15, 30, 59, 0, time.UTC), false, time.Time{}},
		{time.Date(2026, 11, 26, 15, 45, 30, 0, time.UTC), true, time.Date(2026, 11, 26, 15, 45, 0, 0, time.UTC)},
		{now.Add(14 * 24 * time.Hour).Truncate(time.Minute), true, now.Add(14 * 24 * time.Hour).Truncate(time.Minute)},
		{now.Add(14*24*time.Hour + 2*time.Minute), false, time.Time{}},
		{now.Add(-time.Hour), false, time.Time{}},
	}
	for _, c := range cases {
		got, err := normalizeScheduleTime(c.at, now)
		if (err == nil) != c.ok {
			t.Errorf("%v: err = %v, want ok=%v", c.at, err, c.ok)
			continue
		}
		if c.ok && !got.Equal(c.want) {
			t.Errorf("%v -> %v, want %v", c.at, got, c.want)
		}
	}
}

func TestScheduleableChat(t *testing.T) {
	for jid, want := range map[types.JID]bool{
		types.NewJID("60123", types.DefaultUserServer): true,
		types.NewJID("1203", types.GroupServer):        true,
		types.NewJID("999", types.HiddenUserServer):    true,
		types.StatusBroadcastJID:                       false,
		types.NewJID("1203", types.NewsletterServer):   false,
		types.NewJID("123", types.BroadcastServer):     false,
	} {
		if got := scheduleableChat(jid); got != want {
			t.Errorf("%s: %v, want %v", jid, got, want)
		}
	}
}

func TestCheckScheduledSendRejects(t *testing.T) {
	to := types.NewJID("60123", types.DefaultUserServer)
	at := time.Now().Add(time.Hour)
	for name, c := range map[string]struct {
		media, viewOnce bool
		sendAs          string
	}{
		"media":    {media: true},
		"viewOnce": {viewOnce: true},
		"sticker":  {sendAs: "sticker"},
	} {
		if _, err := checkScheduledSend(nil, to, at, c.media, c.viewOnce, c.sendAs); err == nil || !strings.Contains(err.Error(), "text") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := checkScheduledSend(nil, types.NewJID("1", types.NewsletterServer), at, false, false, ""); err == nil {
		t.Error("newsletter accepted")
	}
	if _, err := checkScheduledSend(nil, to, time.Now().Add(5*time.Minute), false, false, ""); err != errScheduleWindow {
		t.Errorf("5 minutes ahead: err = %v", err)
	}
	if _, err := checkScheduledSend(nil, to, at, false, false, ""); err != nil {
		t.Errorf("valid send rejected: %v", err)
	}
}

func newScheduledTestStore(t *testing.T) *MessageStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	prevPG := isPostgres
	isPostgres = false
	t.Cleanup(func() { isPostgres = prevPG })
	for _, q := range []string{
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
			file_enc_sha256 BLOB, file_length INTEGER, quoted_message_id TEXT, quoted_sender TEXT, quoted_content TEXT,
			is_forwarded BOOLEAN NOT NULL DEFAULT 0, direct_path TEXT, scan_status TEXT, scan_reason TEXT, scanned_at TIMESTAMP,
			PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES ('60222@s.whatsapp.net', 'Joy', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	addLifecycleColumns(t, db)
	if err := ensureScheduledTable(db, "BLOB"); err != nil {
		t.Fatal(err)
	}
	return &MessageStore{db: db}
}

// A scheduled row stays out of history until its time, then moves there
// once, at its scheduled time; a cancelled one never does.
func TestScheduledStoreLifecycle(t *testing.T) {
	store := newScheduledTestStore(t)
	chat := "60222@s.whatsapp.net"
	base := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	key := bytes.Repeat([]byte{3}, 32)
	for _, m := range []ScheduledMessage{
		{ID: "A", ChatJID: chat, Content: "first", ScheduledAt: base, KeyID: "ka", RevealKey: key},
		{ID: "B", ChatJID: chat, Content: "second", ScheduledAt: base.Add(time.Hour), KeyID: "kb", RevealKey: key},
		{ID: "C", ChatJID: chat, Content: "cancel me", ScheduledAt: base.Add(30 * time.Minute), KeyID: "kc", RevealKey: key},
	} {
		if err := store.SaveScheduledMessage(m, "60111@s.whatsapp.net"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := store.CountPendingScheduled(chat); n != 3 {
		t.Fatalf("pending = %d", n)
	}
	row, err := store.GetScheduledMessage(chat, "C")
	if err != nil || row.KeyID != "kc" || !bytes.Equal(row.RevealKey, key) || row.Status != scheduledStatusPending {
		t.Fatalf("row C = %+v, %v", row, err)
	}
	if ok, _ := store.SetScheduledStatus(chat, "C", scheduledStatusPending, scheduledStatusCancelled); !ok {
		t.Fatal("cancel did not change C")
	}
	if ok, _ := store.SetScheduledStatus(chat, "C", scheduledStatusPending, scheduledStatusCancelled); ok {
		t.Error("cancelled C twice")
	}
	list, _ := store.ListScheduledMessages(chat, scheduledStatusPending)
	if len(list) != 2 || list[0].ID != "A" || list[1].ID != "B" {
		t.Fatalf("pending list = %+v", list)
	}

	// Before A's time nothing moves.
	if n, err := store.PostDueScheduled(base.Add(-time.Second)); err != nil || n != 0 {
		t.Fatalf("early sweep moved %d (%v)", n, err)
	}
	// At 16:45 A moves, C (cancelled) and B (future) do not.
	if n, err := store.PostDueScheduled(base.Add(45 * time.Minute)); err != nil || n != 1 {
		t.Fatalf("sweep moved %d (%v)", n, err)
	}
	if n, _ := store.PostDueScheduled(base.Add(45 * time.Minute)); n != 0 {
		t.Errorf("second sweep moved %d", n)
	}
	var content string
	var ts time.Time
	var fromMe bool
	if err := store.db.QueryRow(`SELECT content, timestamp, is_from_me FROM messages WHERE id = 'A'`).Scan(&content, &ts, &fromMe); err != nil {
		t.Fatal(err)
	}
	if content != "first" || !ts.Equal(base) || !fromMe {
		t.Errorf("history row = %q %v %v", content, ts, fromMe)
	}
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id IN ('B', 'C')`).Scan(&n)
	if n != 0 {
		t.Errorf("%d unsent rows in history", n)
	}
	all, _ := store.ListScheduledMessages("", "")
	status := map[string]string{}
	for _, m := range all {
		status[m.ID] = m.Status
	}
	if status["A"] != scheduledStatusPosted || status["B"] != scheduledStatusPending || status["C"] != scheduledStatusCancelled {
		t.Errorf("statuses = %v", status)
	}
}

func TestEffectiveScheduledStatus(t *testing.T) {
	now := time.Now()
	if s := effectiveScheduledStatus(ScheduledMessage{Status: scheduledStatusPending, ScheduledAt: now.Add(-time.Minute)}, now); s != scheduledStatusPosted {
		t.Errorf("past pending = %s", s)
	}
	if s := effectiveScheduledStatus(ScheduledMessage{Status: scheduledStatusPending, ScheduledAt: now.Add(time.Minute)}, now); s != scheduledStatusPending {
		t.Errorf("future pending = %s", s)
	}
}

func TestParseChatJID(t *testing.T) {
	for in, want := range map[string]string{
		"60165420926":                "60165420926@s.whatsapp.net",
		"+60165420926":               "60165420926@s.whatsapp.net",
		"60165420926@s.whatsapp.net": "60165420926@s.whatsapp.net",
		"1203@g.us":                  "1203@g.us",
	} {
		got, err := parseChatJID(in)
		if err != nil || got.String() != want {
			t.Errorf("%q -> %v, %v", in, got, err)
		}
	}
	if _, err := parseChatJID("not a number"); err == nil {
		t.Error("accepted garbage")
	}
}

// A time given in another offset is stored and read back as the same
// instant, and reads come back in time.Local.
func TestScheduledTimesAreLocalWallClock(t *testing.T) {
	prevLocal := time.Local
	time.Local = time.FixedZone("MYT", 8*3600)
	defer func() { time.Local = prevLocal }()
	store := newScheduledTestStore(t)
	at := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC) // 15:00 MYT
	if err := store.SaveScheduledMessage(ScheduledMessage{ID: "Z", ChatJID: "60222@s.whatsapp.net", Content: "x", ScheduledAt: at, KeyID: "k"}, "me"); err != nil {
		t.Fatal(err)
	}
	m, err := store.GetScheduledMessage("60222@s.whatsapp.net", "Z")
	if err != nil {
		t.Fatal(err)
	}
	if !m.ScheduledAt.Equal(at) {
		t.Errorf("read back %v, want instant %v", m.ScheduledAt, at)
	}
	if n, _ := store.PostDueScheduled(at.Add(-time.Second)); n != 0 {
		t.Error("posted a second early")
	}
	if n, _ := store.PostDueScheduled(at); n != 1 {
		t.Error("not posted at its time")
	}
}

func TestStoredLocal(t *testing.T) {
	prevLocal := time.Local
	time.Local = time.FixedZone("MYT", 8*3600)
	defer func() { time.Local = prevLocal }()
	got := storedLocal(time.Date(2026, 11, 26, 16, 31, 0, 0, time.UTC))
	if got.Hour() != 16 || got.Location() != time.Local {
		t.Errorf("got %v", got)
	}
}

func TestScheduledNoticeFormat(t *testing.T) {
	prevLocal := time.Local
	time.Local = time.FixedZone("MYT", 8*3600)
	defer func() { time.Local = prevLocal }()
	at := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
	if got := scheduledNotice(scheduledMarker, at); got != "[scheduled message]\nScheduled for: 2026-11-28T15:00:00+08:00" {
		t.Errorf("notice = %q", got)
	}
	if got := scheduledNotice(scheduledMarker, time.Time{}); got != "[scheduled message]" {
		t.Errorf("timeless notice = %q", got)
	}
	for c, want := range map[string]bool{
		"[scheduled message]":                         true,
		"[scheduled message]\nScheduled for: x":       true,
		"[scheduled message cancelled]":               false,
		"[scheduled message] is what I typed, really": false,
		"hello": false,
	} {
		if isScheduledNotice(c) != want {
			t.Errorf("isScheduledNotice(%q) = %v", c, !want)
		}
	}
}

// The notice row stored at send time becomes the message at its time, and a
// cancelled one keeps its time in the cancelled notice.
func TestScheduledNoticeLifecycle(t *testing.T) {
	store := newScheduledTestStore(t)
	chat := "60222@s.whatsapp.net"
	at := time.Date(2026, 11, 26, 16, 31, 0, 0, time.Local)
	sentAt := at.Add(-13 * time.Minute)
	for _, id := range []string{"A", "B"} {
		if err := store.SaveScheduledMessage(ScheduledMessage{ID: id, ChatJID: chat, Content: "text " + id, ScheduledAt: at, KeyID: "k" + id}, "me"); err != nil {
			t.Fatal(err)
		}
		if err := store.StoreMessage(id, chat, "me", scheduledNotice(scheduledMarker, at), sentAt, true,
			"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	store.SetScheduledStatus(chat, "B", scheduledStatusPending, scheduledStatusCancelled)
	store.markScheduledCancelled(chat, "B", at)
	if n, _ := store.PostDueScheduled(at); n != 1 {
		t.Fatalf("posted %d", n)
	}
	read := func(id string) (string, time.Time) {
		var c string
		var ts time.Time
		if err := store.db.QueryRow(`SELECT content, timestamp FROM messages WHERE id = ?`, id).Scan(&c, &ts); err != nil {
			t.Fatal(err)
		}
		return c, ts
	}
	if c, ts := read("A"); c != "text A" || !ts.Equal(at) {
		t.Errorf("A = %q at %v, want the text at %v", c, ts, at)
	}
	if c, ts := read("B"); c != scheduledNotice(scheduledCancelledMarker, at) || !ts.Equal(sentAt) {
		t.Errorf("B = %q at %v", c, ts)
	}
	// Cancelling again, or a non-notice row, changes nothing.
	store.markScheduledCancelled(chat, "A", at)
	if c, _ := read("A"); c != "text A" {
		t.Errorf("A rewritten to %q", c)
	}
}

func TestHandleScheduledIncoming(t *testing.T) {
	store := newScheduledTestStore(t)
	chat := "60222@s.whatsapp.net"
	cr := &waE2E.Message{ConditionalRevealMessage: &waE2E.ConditionalRevealMessage{RevealKeyID: proto.String("k")}}
	evt := func(id string, fromMe bool, m *waE2E.Message) *events.Message {
		e := &events.Message{Message: m}
		e.Info.ID, e.Info.IsFromMe, e.Info.Timestamp = id, fromMe, time.Date(2026, 11, 26, 16, 0, 0, 0, time.Local)
		return e
	}
	if !handleScheduledIncoming(store, evt("PHONE", true, cr), chat, "me") {
		t.Fatal("own scheduled message not handled")
	}
	if !handleScheduledIncoming(store, evt("THEIRS", false, cr), chat, "60222") {
		t.Fatal("their scheduled message not handled")
	}
	var c string
	if err := store.db.QueryRow(`SELECT content FROM messages WHERE id = 'PHONE'`).Scan(&c); err != nil || c != scheduledMarker {
		t.Errorf("own notice = %q, %v", c, err)
	}
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'THEIRS'`).Scan(&n)
	if n != 0 {
		t.Error("stored a row for someone else's unrevealed message")
	}
	unschedule := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("PHONE"), FromMe: proto.Bool(true)},
		Type: waE2E.ProtocolMessage_MESSAGE_UNSCHEDULE.Enum(),
	}}
	if !handleScheduledIncoming(store, evt("U1", true, unschedule), chat, "me") {
		t.Fatal("unschedule not handled")
	}
	store.db.QueryRow(`SELECT content FROM messages WHERE id = 'PHONE'`).Scan(&c)
	if c != scheduledCancelledMarker {
		t.Errorf("after cancel = %q", c)
	}
	if handleScheduledIncoming(store, evt("T", false, &waE2E.Message{Conversation: proto.String("hi")}), chat, "60222") {
		t.Error("plain text treated as scheduled")
	}
}
