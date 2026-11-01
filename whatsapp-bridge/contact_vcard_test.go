package main

import "testing"

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
