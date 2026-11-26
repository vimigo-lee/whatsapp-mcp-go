package main

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// Photos and videos go out as ViewOnceMessageV2, voice notes as
// ViewOnceMessageV2Extension, each with the inline viewOnce flag kept on the
// media and its context info (mentions, reply) still attached.
func TestWrapViewOnceShape(t *testing.T) {
	ctx := &waE2E.ContextInfo{StanzaID: proto.String("QUOTED")}
	cases := []struct {
		name    string
		msg     *waE2E.Message
		wantExt bool
		leaf    func(*waE2E.Message) (viewOnce bool, ok bool, c *waE2E.ContextInfo)
	}{
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("hi"), ContextInfo: ctx}}, false,
			func(m *waE2E.Message) (bool, bool, *waE2E.ContextInfo) {
				return m.GetImageMessage().GetViewOnce(), m.GetImageMessage() != nil, m.GetImageMessage().GetContextInfo()
			}},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{ContextInfo: ctx}}, false,
			func(m *waE2E.Message) (bool, bool, *waE2E.ContextInfo) {
				return m.GetVideoMessage().GetViewOnce(), m.GetVideoMessage() != nil, m.GetVideoMessage().GetContextInfo()
			}},
		{"voice note", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), ContextInfo: ctx}}, true,
			func(m *waE2E.Message) (bool, bool, *waE2E.ContextInfo) {
				return m.GetAudioMessage().GetViewOnce(), m.GetAudioMessage() != nil, m.GetAudioMessage().GetContextInfo()
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapViewOnce(c.msg)
			if got == nil {
				t.Fatal("wrapViewOnce returned nil")
			}
			if got.ImageMessage != nil || got.VideoMessage != nil || got.AudioMessage != nil {
				t.Fatal("media must sit inside the wrapper, not at the top level")
			}
			var inner *waE2E.Message
			if c.wantExt {
				if got.ViewOnceMessageV2 != nil || got.ViewOnceMessage != nil {
					t.Fatal("voice note must use only ViewOnceMessageV2Extension")
				}
				inner = got.GetViewOnceMessageV2Extension().GetMessage()
			} else {
				if got.ViewOnceMessageV2Extension != nil || got.ViewOnceMessage != nil {
					t.Fatal("photo/video must use only ViewOnceMessageV2")
				}
				inner = got.GetViewOnceMessageV2().GetMessage()
			}
			viewOnce, ok, gotCtx := c.leaf(inner)
			if !ok || !viewOnce {
				t.Fatalf("leaf present=%v viewOnce=%v, want both true", ok, viewOnce)
			}
			if gotCtx.GetStanzaID() != "QUOTED" {
				t.Error("context info lost in the wrap")
			}
			// The bridge's own unwrap (used for stored/incoming rows) still
			// reaches the media.
			if unwrapMessage(got) != inner {
				t.Error("unwrapMessage does not reach the wrapped media")
			}
		})
	}
	for _, m := range []*waE2E.Message{
		{DocumentMessage: &waE2E.DocumentMessage{}},
		{StickerMessage: &waE2E.StickerMessage{}},
		{Conversation: proto.String("text")},
	} {
		if wrapViewOnce(m) != nil {
			t.Errorf("wrapViewOnce(%v) should be nil", m)
		}
	}
}

func TestCheckViewOnce(t *testing.T) {
	ok := []struct {
		mt   whatsmeow.MediaType
		mime string
	}{
		{whatsmeow.MediaImage, "image/jpeg"},
		{whatsmeow.MediaImage, "image/png"},
		{whatsmeow.MediaVideo, "video/mp4"},
		{whatsmeow.MediaAudio, voiceNoteMimetype},
	}
	for _, c := range ok {
		if e := checkViewOnce("", c.mt, c.mime); e != "" {
			t.Errorf("%s %s: rejected: %s", c.mt, c.mime, e)
		}
		if e := checkViewOnce("media", c.mt, c.mime); e != "" {
			t.Errorf("%s %s with send_as=media: rejected: %s", c.mt, c.mime, e)
		}
	}
	bad := []struct {
		sendAs string
		mt     whatsmeow.MediaType
		mime   string
		want   string
	}{
		{"", whatsmeow.MediaDocument, "application/pdf", "not documents"},
		{"", whatsmeow.MediaImage, "image/gif", "GIF"},
		{"gif", whatsmeow.MediaVideo, "video/mp4", "send_as=gif"},
		{"sticker", whatsmeow.MediaImage, "image/webp", "send_as=sticker"},
	}
	for _, c := range bad {
		e := checkViewOnce(c.sendAs, c.mt, c.mime)
		if !strings.Contains(e, c.want) {
			t.Errorf("send_as=%q %s %s: got %q, want it to mention %q", c.sendAs, c.mt, c.mime, e, c.want)
		}
	}
}

// view_once without media is refused before anything touches the client.
func TestSendViewOnceTextRefused(t *testing.T) {
	ok, msg, id := sendWhatsAppMessage(nil, nil, "60123456789", "hello", "", "", "", "", nil, "", "", "", false, true, "", true)
	if ok || id != "" || !strings.Contains(msg, "view_once") {
		t.Fatalf("got ok=%v id=%q msg=%q, want a view_once refusal", ok, id, msg)
	}
}

func TestViewOnceMetaAndStoredContent(t *testing.T) {
	n := viewOnceMetaNode()
	if n.Tag != "meta" || n.Attrs["view_once"] != "true" || len(n.Attrs) != 1 {
		t.Fatalf("meta node = %+v, want <meta view_once=\"true\"/>", n)
	}
	if got := viewOnceContent(""); got != "[view once]" {
		t.Errorf("no caption: %q", got)
	}
	if got := viewOnceContent("look"); got != "[view once] look" {
		t.Errorf("caption: %q", got)
	}
}
