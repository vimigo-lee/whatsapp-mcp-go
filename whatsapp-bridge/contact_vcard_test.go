package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestBuildContactVCard(t *testing.T) {
	got := buildContactVCard("Tan, Ah; Kow", []string{"60123456789", "6598765432"})
	want := "BEGIN:VCARD\nVERSION:3.0\n" +
		"N:;Tan\\, Ah\\; Kow;;;\n" +
		"FN:Tan\\, Ah\\; Kow\n" +
		"TEL;type=CELL;type=VOICE;waid=60123456789:+60123456789\n" +
		"TEL;type=CELL;type=VOICE;waid=6598765432:+6598765432\n" +
		"END:VCARD"
	if got != want {
		t.Fatalf("vcard mismatch\n got: %q\nwant: %q", got, want)
	}
}

// iOS shares cards with CRLF line ends and "item1." grouped TEL lines.
const iosVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Lim;Mei Ling;;;\r\nFN:Mei Ling Lim\r\n" +
	"item1.TEL;waid=60198765432:+60 19-876 5432\r\nitem1.X-ABLabel:Mobile\r\nEND:VCARD"

func TestContactCardContent(t *testing.T) {
	single := &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
		DisplayName: proto.String("Mei Ling Lim"),
		Vcard:       proto.String(iosVCard + "\r\n"),
	}}
	if got := contactCardContent(single); got != iosVCard {
		t.Fatalf("single card\n got: %q\nwant: %q", got, iosVCard)
	}

	other := buildContactVCard("Ali", []string{"60123456789"})
	multi := &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
		Contacts: []*waE2E.ContactMessage{
			{Vcard: proto.String(iosVCard)},
			{Vcard: proto.String("")},
			{Vcard: proto.String(other)},
		},
	}}
	if got, want := contactCardContent(multi), iosVCard+"\n"+other; got != want {
		t.Fatalf("contacts array\n got: %q\nwant: %q", got, want)
	}

	// No vCard → "" so the caller falls back to the "[contact]" label.
	if got := contactCardContent(&waE2E.Message{ContactMessage: &waE2E.ContactMessage{}}); got != "" {
		t.Fatalf("empty card: got %q", got)
	}
	if got := contactCardContent(&waE2E.Message{Conversation: proto.String("hi")}); got != "" {
		t.Fatalf("text message: got %q", got)
	}
}

func TestContactCardSummary(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"built card", buildContactVCard("Tan, Ah; Kow", []string{"60123456789", "6598765432"}),
			"[contact] Tan, Ah; Kow (+60123456789, +6598765432)"},
		{"ios card prefers waid", iosVCard, "[contact] Mei Ling Lim (+60198765432)"},
		{"tel without waid", "BEGIN:VCARD\nVERSION:3.0\nFN:Shop\nTEL;TYPE=WORK:03-1234 5678\nEND:VCARD",
			"[contact] Shop (03-1234 5678)"},
		{"several cards", iosVCard + "\n" + buildContactVCard("Ali", []string{"60123456789"}),
			"[contact] Mei Ling Lim (+60198765432) [contact] Ali (+60123456789)"},
		{"plain text untouched", "see BEGIN:VCARD below", "see BEGIN:VCARD below"},
		{"label untouched", "[contact]", "[contact]"},
	}
	for _, c := range cases {
		if got := contactCardSummary(c.in); got != c.want {
			t.Errorf("%s\n got: %q\nwant: %q", c.name, got, c.want)
		}
	}
}
