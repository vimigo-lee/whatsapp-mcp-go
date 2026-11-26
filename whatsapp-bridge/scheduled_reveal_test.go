package main

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func TestParseScheduledMex(t *testing.T) {
	key := bytes.Repeat([]byte{0xfb}, 32) // url-safe base64 differs from std here
	rk := base64.RawURLEncoding.EncodeToString(key)
	for _, st := range []string{`1764150000`, `"1764150000"`} {
		r, p, err := parseScheduledMex([]byte(`{"data":{"xwa2_notify_scheduled_message_reveal":{"chat_jid":"60111@s.whatsapp.net","rk":"` + rk + `","rkid":"abc","st":` + st + `,"sender_user_jid":"60111@s.whatsapp.net"}}}`))
		if err != nil || p != nil || r == nil {
			t.Fatalf("reveal st=%s: %v %v %v", st, r, p, err)
		}
		if r.KeyID != "abc" || int64(r.Time) != 1764150000 || r.SenderJID != "60111@s.whatsapp.net" {
			t.Errorf("reveal = %+v", r)
		}
		got, err := decodeRevealKey(r.Key)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("key = %x, %v", got, err)
		}
	}
	r, p, err := parseScheduledMex([]byte(`{"data":{"xwa2_notify_scheduled_message_post":{"chat_jid":"x","rkid":"abc","status":"SUCCESS"}}}`))
	if err != nil || r != nil || p == nil || p.KeyID != "abc" || p.Status != "SUCCESS" {
		t.Errorf("post = %+v %+v %v", r, p, err)
	}
	if _, err := decodeRevealKey("dG9vIHNob3J0"); err == nil {
		t.Error("accepted a short key")
	}
}

// sealedFor returns a sealed scheduled message and its key and key ID.
func sealedFor(t *testing.T, text string) (*waE2E.Message, []byte, string) {
	t.Helper()
	s, err := sealScheduledMessage(&waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		t.Fatal(err)
	}
	return s.Outer, s.RevealKey, s.KeyID
}

func incomingEvt(id string, fromMe bool, sender types.JID, m *waE2E.Message) *events.Message {
	e := &events.Message{Message: m}
	e.Info.ID, e.Info.IsFromMe, e.Info.Sender = id, fromMe, sender
	e.Info.Chat = types.NewJID("60222", types.DefaultUserServer)
	e.Info.PushName = "Lee"
	e.Info.Timestamp = time.Date(2026, 11, 26, 16, 0, 0, 0, time.Local)
	return e
}

// Someone else's scheduled message: nothing shows on arrival; once its key
// is here (in either order) it opens into the original message, with its own
// ID, sender and the scheduled time.
func TestIncomingScheduledReveal(t *testing.T) {
	store := newScheduledTestStore(t)
	chat := "60222@s.whatsapp.net"
	lee := types.NewJID("60222", types.DefaultUserServer)
	at := time.Date(2026, 11, 26, 17, 31, 0, 0, time.Local)
	now := at.Add(time.Second)

	// Ciphertext first, then the key.
	outer, key, keyID := sealedFor(t, "4554")
	if !handleScheduledIncoming(nil, store, incomingEvt("M1", false, lee, outer), chat, "60222", waLog.Noop) {
		t.Fatal("not handled")
	}
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = 'M1'`).Scan(&n)
	if n != 0 {
		t.Error("a row showed before the reveal")
	}
	r, _ := store.getIncoming(keyID)
	if _, err := revealedMessage(r, now); err != errRevealNotReady {
		t.Errorf("revealable without its key: %v", err)
	}
	if err := store.saveIncomingKey(keyID, key, at); err != nil {
		t.Fatal(err)
	}
	r, _ = store.getIncoming(keyID)
	evt, err := revealedMessage(r, now)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Message.GetConversation() != "4554" || evt.Info.ID != "M1" || evt.Info.Sender != lee ||
		evt.Info.Chat.String() != chat || evt.Info.IsFromMe || !evt.Info.Timestamp.Equal(at) || evt.Info.PushName != "Lee" {
		t.Errorf("revealed = %q %+v", evt.Message.GetConversation(), evt.Info)
	}
	// A reveal is claimed once.
	if ok, _ := store.setIncomingStatus(keyID, incomingPending, incomingRevealed); !ok {
		t.Fatal("claim failed")
	}
	if ok, _ := store.setIncomingStatus(keyID, incomingPending, incomingRevealed); ok {
		t.Error("claimed twice")
	}

	// Key first (offline replay), then the ciphertext.
	outer2, key2, keyID2 := sealedFor(t, "4444")
	if err := store.saveIncomingKey(keyID2, key2, at); err != nil {
		t.Fatal(err)
	}
	handleScheduledIncoming(nil, store, incomingEvt("M2", false, lee, outer2), chat, "60222", waLog.Noop)
	r, _ = store.getIncoming(keyID2)
	if evt, err := revealedMessage(r, now); err != nil || evt.Message.GetConversation() != "4444" || evt.Info.ID != "M2" {
		t.Errorf("key-first reveal = %v, %v", evt, err)
	}

	// Taken back by its sender: never revealed.
	outer3, key3, keyID3 := sealedFor(t, "never")
	handleScheduledIncoming(nil, store, incomingEvt("M3", false, lee, outer3), chat, "60222", waLog.Noop)
	unschedule := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("M3")},
		Type: waE2E.ProtocolMessage_MESSAGE_UNSCHEDULE.Enum(),
	}}
	handleScheduledIncoming(nil, store, incomingEvt("U3", false, lee, unschedule), chat, "60222", waLog.Noop)
	store.saveIncomingKey(keyID3, key3, at)
	r, _ = store.getIncoming(keyID3)
	if _, err := revealedMessage(r, now); err != errRevealNotReady {
		t.Errorf("cancelled message revealable: %v", err)
	}
}

// One scheduled on the owner's phone arrives with its key and time (patched
// whatsmeow): it is listed with its text, gets a notice saying when, is not
// posted by the bridge's own sweeper, and is due for reveal after its time.
func TestOwnPhoneScheduled(t *testing.T) {
	store := newScheduledTestStore(t)
	chat := "60222@s.whatsapp.net"
	me := types.NewJID("60111", types.DefaultUserServer)
	at := time.Date(2026, 11, 26, 18, 0, 0, 0, time.Local)
	outer, key, keyID := sealedFor(t, "from the phone")

	prev := scheduledMetaOf
	scheduledMetaOf = func(*events.Message) (string, []byte, time.Time) { return keyID, key, at }
	defer func() { scheduledMetaOf = prev }()

	handleScheduledIncoming(nil, store, incomingEvt("P1", true, me, outer), chat, "60111", waLog.Noop)

	list, _ := store.ListScheduledMessages(chat, scheduledStatusPending)
	if len(list) != 1 || list[0].ID != "P1" || list[0].Content != "from the phone" || list[0].Source != scheduledSourcePhone || !list[0].ScheduledAt.Equal(at) {
		t.Fatalf("listed = %+v", list)
	}
	var notice string
	store.db.QueryRow(`SELECT content FROM messages WHERE id = 'P1'`).Scan(&notice)
	if notice != scheduledNotice(scheduledMarker, at, "from the phone") {
		t.Errorf("notice = %q", notice)
	}
	if n, _ := store.PostDueScheduled(at.Add(time.Hour)); n != 0 {
		t.Error("the bridge's sweeper posted a phone-scheduled message")
	}
	if ids, _ := store.dueOwnIncoming(at.Add(ownRevealGrace - time.Second)); len(ids) != 0 {
		t.Errorf("due before its grace: %v", ids)
	}
	if ids, _ := store.dueOwnIncoming(at.Add(ownRevealGrace + time.Second)); len(ids) != 1 || ids[0] != keyID {
		t.Errorf("due = %v", ids)
	}
	r, _ := store.getIncoming(keyID)
	if evt, err := revealedMessage(r, at.Add(time.Hour)); err != nil || !evt.Info.IsFromMe || evt.Message.GetConversation() != "from the phone" {
		t.Errorf("own reveal = %v, %v", evt, err)
	}
}

// WhatsApp's post notification with a failure marks one of our scheduled
// messages failed.
func TestScheduledPostFailure(t *testing.T) {
	store := newScheduledTestStore(t)
	if _, err := store.db.Exec(`ALTER TABLE messages ADD COLUMN failed_at TIMESTAMP`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE messages ADD COLUMN failure_reason TEXT`); err != nil {
		t.Fatal(err)
	}
	chat := "60222@s.whatsapp.net"
	at := time.Date(2026, 11, 26, 18, 0, 0, 0, time.Local)
	store.SaveScheduledMessage(ScheduledMessage{ID: "B1", ChatJID: chat, Content: "x", ScheduledAt: at, KeyID: "kb1"}, "me")
	store.StoreMessage("B1", chat, "me", "x", at, true, "", "", "", "", nil, nil, nil, 0, "", "", "", false)

	handleScheduledPost(nil, store, &scheduledPost{KeyID: "kb1", Status: "ERROR", ErrorCode: []byte(`"419"`)}, waLog.Noop)

	row, _ := store.GetScheduledMessage(chat, "B1")
	if row.Status != scheduledStatusFailed {
		t.Errorf("status = %q", row.Status)
	}
	var reason string
	store.db.QueryRow(`SELECT failure_reason FROM messages WHERE id = 'B1'`).Scan(&reason)
	if reason == "" {
		t.Error("message row not marked failed")
	}
}
