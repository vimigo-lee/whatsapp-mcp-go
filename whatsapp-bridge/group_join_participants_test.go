package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// fakeLIDs answers only the two lookups joinParticipants makes.
type fakeLIDs struct {
	wastore.LIDStore
	pnByLID map[string]types.JID
	lidByPN map[string]types.JID
}

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	return f.pnByLID[lid.User], nil
}

func (f fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	return f.lidByPN[pn.User], nil
}

func TestJoinParticipantsGivesBothAddresses(t *testing.T) {
	pn := types.NewJID("60123456789", types.DefaultUserServer)
	lid := types.NewJID("111222333", types.HiddenUserServer)
	client := &whatsmeow.Client{Store: &wastore.Device{LIDs: fakeLIDs{
		pnByLID: map[string]types.JID{lid.User: pn},
		lidByPN: map[string]types.JID{pn.User: lid},
	}}}

	unknownLID := types.NewJID("999888777", types.HiddenUserServer)
	unknownPN := types.NewJID("60198765432", types.DefaultUserServer)

	got := joinParticipants(client, []types.JID{lid, pn, unknownLID, unknownPN})
	want := []map[string]string{
		{"phone_number": "60123456789", "lid": "111222333@lid"}, // named by LID
		{"phone_number": "60123456789", "lid": "111222333@lid"}, // named by phone
		{"phone_number": "", "lid": "999888777@lid"},            // number hidden
		{"phone_number": "60198765432", "lid": ""},              // LID not known
	}
	if len(got) != len(want) {
		t.Fatalf("got %d participants, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i]["phone_number"] != want[i]["phone_number"] || got[i]["lid"] != want[i]["lid"] {
			t.Errorf("participant %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestJoinParticipantsWithoutLIDStore(t *testing.T) {
	lid := types.NewJID("111222333", types.HiddenUserServer)
	got := joinParticipants(&whatsmeow.Client{Store: &wastore.Device{}}, []types.JID{lid})
	if len(got) != 1 || got[0]["phone_number"] != "" || got[0]["lid"] != "111222333@lid" {
		t.Fatalf("got %v", got)
	}
}
