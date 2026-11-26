package main

import (
	"testing"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func TestGroupInfoParticipantFillsPhoneFromLIDStore(t *testing.T) {
	pn := types.NewJID("60123456789", types.DefaultUserServer)
	lid := types.NewJID("111222333", types.HiddenUserServer)
	client := &whatsmeow.Client{Store: &wastore.Device{LIDs: fakeLIDs{
		pnByLID: map[string]types.JID{lid.User: pn},
	}}}

	cases := []struct {
		name string
		in   types.GroupParticipant
		want string
	}{
		{"server gave number", types.GroupParticipant{JID: lid, LID: lid, PhoneNumber: types.NewJID("60111111111", types.DefaultUserServer)}, "60111111111"},
		{"lid only, mapped", types.GroupParticipant{JID: lid, LID: lid}, "60123456789"},
		{"lid only, unknown", types.GroupParticipant{JID: types.NewJID("999", types.HiddenUserServer)}, ""},
		{"pn-addressed group", types.GroupParticipant{JID: pn}, "60123456789"},
	}
	for _, c := range cases {
		if got := groupInfoParticipant(client, c.in)["phone_number"]; got != c.want {
			t.Errorf("%s: phone_number = %q, want %q", c.name, got, c.want)
		}
	}
}
