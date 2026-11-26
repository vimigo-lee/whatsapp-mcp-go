package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func strp(s string) *string { return &s }

func TestApplyEventEditChangesOnlyNamedFields(t *testing.T) {
	base := &waE2E.EventMessage{
		Name:        proto.String("Launch"),
		Description: proto.String("Bring snacks"),
		StartTime:   proto.Int64(1_800_000_000),
		EndTime:     proto.Int64(1_800_003_600),
		Location:    &waE2E.LocationMessage{Name: proto.String("HQ")},
		IsCanceled:  proto.Bool(false),
	}
	ev, err := applyEventEdit(base, EditEventRequest{Name: strp("Launch v2"), EndTime: strp("")})
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetName() != "Launch v2" || ev.GetDescription() != "Bring snacks" || ev.GetLocation().GetName() != "HQ" {
		t.Errorf("unchanged fields lost: %+v", ev)
	}
	if ev.EndTime != nil {
		t.Errorf("empty end_time should remove the end, got %d", ev.GetEndTime())
	}
	if base.GetName() != "Launch" || base.EndTime == nil {
		t.Errorf("base was modified: %+v", base)
	}

	if _, err := applyEventEdit(base, EditEventRequest{EndTime: strp("2020-01-01T00:00:00Z")}); err == nil {
		t.Error("an end before the start must be refused")
	}
	if _, err := applyEventEdit(base, EditEventRequest{Name: strp("  ")}); err == nil {
		t.Error("an empty name must be refused")
	}
	if ev, _ := applyEventEdit(base, EditEventRequest{Canceled: true}); !ev.GetIsCanceled() || ev.GetName() != "Launch" {
		t.Errorf("cancel should keep the event and set is_canceled: %+v", ev)
	}
}

// The edit our bridge sends must decrypt on the recipient's side into the new
// event, and a bridge receiving it must update the card and remember it.
func TestEventEditRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })

	own := types.NewJID("60100000000", types.DefaultUserServer)
	friend := types.NewJID("60111111111", types.DefaultUserServer)
	secrets := &fakeSecrets{id: "EV1", sender: own, secret: []byte(strings.Repeat("k", 32))}
	client := &whatsmeow.Client{Store: &wastore.Device{ID: &own, MsgSecrets: secrets}}

	t0 := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
	orig := &waE2E.EventMessage{Name: proto.String("Launch"), StartTime: proto.Int64(t0.Unix()), IsCanceled: proto.Bool(false)}

	// Recipient side: the friend's bridge holds the event as received from us.
	if err := ms.StoreChat(own.String(), "Me", t0); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("EV1", own.String(), own.User, eventContent(&waE2E.Message{EventMessage: orig}), t0, false,
		"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}

	edited, err := applyEventEdit(orig, EditEventRequest{Name: strp("Launch party"), Location: strp("Rooftop")})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := buildEventEditMessage(context.Background(), client, friend, "EV1", edited)
	if err != nil {
		t.Fatal(err)
	}
	sem := msg.GetSecretEncryptedMessage()
	if sem.GetSecretEncType() != waE2E.SecretEncryptedMessage_EVENT_EDIT || sem.GetTargetMessageKey().GetID() != "EV1" || !sem.GetTargetMessageKey().GetFromMe() {
		t.Fatalf("unexpected envelope: %+v", sem)
	}

	// As the friend receives it: from us, in the DM with us.
	evt := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: own, Sender: own},
		ID:            "EDIT1", Timestamp: t0.Add(time.Minute),
	}, Message: msg}
	recipient := &whatsmeow.Client{Store: &wastore.Device{ID: &friend, MsgSecrets: secrets}}
	plain, err := recipient.DecryptSecretEncryptedMessage(context.Background(), evt)
	if err != nil {
		t.Fatalf("recipient cannot decrypt the edit: %v", err)
	}
	pm := plain.GetEditedMessage().GetMessage().GetProtocolMessage()
	if pm.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT || pm.GetKey().GetID() != "EV1" {
		t.Fatalf("decrypted edit is not a MESSAGE_EDIT of EV1: %+v", plain)
	}
	if got := pm.GetEditedMessage().GetEventMessage(); got.GetName() != "Launch party" || got.GetLocation().GetName() != "Rooftop" {
		t.Fatalf("decrypted event = %+v", got)
	}

	handleMessage(recipient, ms, evt, waLog.Noop)
	var content string
	if err := ms.db.QueryRow(`SELECT content FROM messages WHERE id = 'EV1'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(content, "[event] Launch party") || !strings.Contains(content, "Where: Rooftop") {
		t.Errorf("stored card not updated: %q", content)
	}
	saved, err := ms.LoadEventMessage("EV1")
	if err != nil || saved.GetName() != "Launch party" {
		t.Errorf("edited event not remembered: %+v %v", saved, err)
	}
}

func TestEventEditNeedsKnownEvent(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })
	if _, err := ms.LoadEventMessage("nope"); err != errEventNotFound {
		t.Errorf("err = %v, want errEventNotFound", err)
	}
}

func TestEditEventFallsBackToRequestForUnknownEvent(t *testing.T) {
	base := &waE2E.EventMessage{IsCanceled: proto.Bool(false)}
	ev, err := applyEventEdit(base, EditEventRequest{Name: strp("Old event"), StartTime: strp("2026-11-28T15:00:00+08:00"), Canceled: true})
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetName() != "Old event" || ev.GetStartTime() != 1795849200 || !ev.GetIsCanceled() {
		t.Errorf("event = %+v", ev)
	}
}

func TestReceivedInviteKeepsCover(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })

	own := types.NewJID("60100000000", types.DefaultUserServer)
	friend := types.NewJID("60111111111", types.DefaultUserServer)
	client := &whatsmeow.Client{Store: &wastore.Device{ID: &own}}
	now := time.Now()
	if err := ms.StoreChat(friend.String(), "Friend", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	inv := &waE2E.EventInviteMessage{EventTitle: proto.String("Dinner"), StartTime: proto.Int64(now.Add(24 * time.Hour).Unix()), JPEGThumbnail: []byte{0xff, 0xd8, 0xff}}
	handleMessage(client, ms, &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: friend, Sender: friend}, ID: "INV1", Timestamp: now},
		Message: &waE2E.Message{EventInviteMessage: inv},
	}, waLog.Noop)

	var content string
	if err := ms.db.QueryRow(`SELECT content FROM messages WHERE id = 'INV1'`).Scan(&content); err != nil || !strings.HasPrefix(content, "[event] Dinner") {
		t.Fatalf("invite not stored as an event card: %q %v", content, err)
	}
	invite, cover := ms.EventInviteInfo("INV1")
	if !invite || cover != "/9j/" {
		t.Errorf("invite = %v, cover = %q", invite, cover)
	}
	if invite, _ := ms.EventInviteInfo("nope"); invite {
		t.Error("unknown event reported as invite")
	}
}
