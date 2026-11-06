package main

import (
	"testing"

	"go.mau.fi/whatsmeow"
)

// animated VP8X header for a 512x512 canvas (w-1 = h-1 = 511 = 0x0001ff).
func vp8xHeader(animated bool) []byte {
	b := make([]byte, 30)
	copy(b[0:], "RIFF")
	copy(b[8:], "WEBP")
	copy(b[12:], "VP8X")
	if animated {
		b[20] = 0x02
	}
	b[24], b[25], b[26] = 0xff, 0x01, 0x00
	b[27], b[28], b[29] = 0xff, 0x01, 0x00
	return b
}

func TestWebPInfoVP8X(t *testing.T) {
	w, h, anim := webpInfo(vp8xHeader(true))
	if w != 512 || h != 512 || !anim {
		t.Fatalf("got %dx%d animated=%v, want 512x512 animated", w, h, anim)
	}
	if _, _, anim := webpInfo(vp8xHeader(false)); anim {
		t.Fatal("static VP8X reported as animated")
	}
	if w, h, _ := webpInfo([]byte("not a webp at all, definitely not")); w != 0 || h != 0 {
		t.Fatal("non-WebP should report zero size")
	}
}

func TestCheckSendAs(t *testing.T) {
	webp := vp8xHeader(true)
	cases := []struct {
		name    string
		sendAs  string
		mt      whatsmeow.MediaType
		mime    string
		data    []byte
		caption string
		ok      bool
	}{
		{"default", "", whatsmeow.MediaImage, "image/png", nil, "", true},
		{"gif mp4", "gif", whatsmeow.MediaVideo, "video/mp4", nil, "hi", true},
		{"gif from .gif rejected", "gif", whatsmeow.MediaImage, "image/gif", nil, "", false},
		{"gif from mov rejected", "gif", whatsmeow.MediaVideo, "video/quicktime", nil, "", false},
		{"sticker webp", "sticker", whatsmeow.MediaImage, "image/webp", webp, "", true},
		{"sticker png rejected", "sticker", whatsmeow.MediaImage, "image/png", []byte("\x89PNG\r\n\x1a\n0000000000000000000000"), "", false},
		{"sticker sent as document rejected", "sticker", whatsmeow.MediaDocument, "application/octet-stream", webp, "", false},
		{"sticker caption rejected", "sticker", whatsmeow.MediaImage, "image/webp", webp, "hi", false},
		{"sticker too big", "sticker", whatsmeow.MediaImage, "image/webp", append(webp, make([]byte, maxStickerBytes)...), "", false},
		{"unknown", "voice", whatsmeow.MediaImage, "image/png", nil, "", false},
	}
	for _, c := range cases {
		got := checkSendAs(c.sendAs, c.mt, c.mime, c.data, c.caption)
		if (got == "") != c.ok {
			t.Errorf("%s: got %q, want ok=%v", c.name, got, c.ok)
		}
	}
}
