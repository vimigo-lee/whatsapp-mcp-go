package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// The debug dump keeps a message's shape readable but never prints the keys
// that decrypt its media or edits.
func TestRedactedMessageJSONHidesKeys(t *testing.T) {
	m := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String("hello"),
			MediaKey:      []byte("MEDIA-KEY-BYTES-0123456789abcdef"),
			FileSHA256:    []byte("SHA-BYTES"),
			DirectPath:    proto.String("/v/t62/secret-path"),
			URL:           proto.String("https://mmg.whatsapp.net/secret"),
			JPEGThumbnail: []byte("THUMB"),
		},
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: []byte("MSG-SECRET-BYTES")},
	}
	got := redactedMessageJSON(m)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "imageMessage") {
		t.Errorf("structure lost: %s", got)
	}
	for _, leak := range []string{"TUVESUEtS0VZ", "U0hBLUJZVEVT", "secret-path", "mmg.whatsapp.net", "VEhVTUI=", "TVNHLVNFQ1JFVC1CWVRFUw=="} {
		if strings.Contains(got, leak) {
			t.Errorf("debug dump leaks %q: %s", leak, got)
		}
	}
}
