package main

import (
	"strings"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// checkViewOnce validates a view_once send. View-once is a photo, video or
// voice-note feature: WhatsApp's own clients only ever mark those media kinds
// view-once (WA Web's genMetaNode only emits <meta view_once> for image, video,
// audio and PTV records), so documents, stickers and GIFs are refused rather
// than sent as something the recipient's app may not render.
func checkViewOnce(sendAs string, mediaType whatsmeow.MediaType, mimeType string) string {
	switch sendAs {
	case "gif", "sticker":
		return "view_once cannot be combined with send_as=" + sendAs
	}
	switch mediaType {
	case whatsmeow.MediaImage:
		if strings.Contains(mimeType, "gif") {
			return "view_once does not support GIFs; send a JPEG/PNG photo or an MP4 video"
		}
		return ""
	case whatsmeow.MediaVideo, whatsmeow.MediaAudio:
		return ""
	}
	return "view_once works only for photos, videos and voice notes, not documents"
}

// viewOnceMetaNode is the stanza child WhatsApp clients attach to every
// view-once media send (<meta view_once="true"/>). whatsmeow does not derive
// it from the protobuf, so the bridge passes it as an additional node.
func viewOnceMetaNode() waBinary.Node {
	return waBinary.Node{Tag: "meta", Attrs: waBinary.Attrs{"view_once": "true"}}
}

// wrapViewOnce turns a built image, video or voice-note message into its
// view-once form, the way WhatsApp's clients build it: the media keeps its
// inline viewOnce=true flag and sits inside a FutureProof wrapper —
// ViewOnceMessageV2 for photos and videos, ViewOnceMessageV2Extension for
// voice notes. It returns nil for anything else.
func wrapViewOnce(msg *waE2E.Message) *waE2E.Message {
	inner := &waE2E.Message{}
	switch {
	case msg.GetImageMessage() != nil:
		inner.ImageMessage = msg.ImageMessage
		inner.ImageMessage.ViewOnce = proto.Bool(true)
		return &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: inner}}
	case msg.GetVideoMessage() != nil:
		inner.VideoMessage = msg.VideoMessage
		inner.VideoMessage.ViewOnce = proto.Bool(true)
		return &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: inner}}
	case msg.GetAudioMessage() != nil:
		inner.AudioMessage = msg.AudioMessage
		inner.AudioMessage.ViewOnce = proto.Bool(true)
		return &waE2E.Message{ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{Message: inner}}
	}
	return nil
}

// viewOnceContent is the text stored for a sent view-once message, so
// list_messages shows it as view-once: "[view once]" plus any caption.
func viewOnceContent(caption string) string {
	if caption == "" {
		return "[view once]"
	}
	return "[view once] " + caption
}

// isViewOnceEvent reports whether a received (or phone-sent) message is
// view-once: wrapped in a ViewOnce message, or media flagged view_once.
func isViewOnceEvent(evt *events.Message) bool {
	if evt == nil {
		return false
	}
	if evt.IsViewOnce || evt.IsViewOnceV2 || evt.IsViewOnceV2Extension {
		return true
	}
	m := unwrapMessage(evt.Message)
	return m.GetImageMessage().GetViewOnce() || m.GetVideoMessage().GetViewOnce() || m.GetAudioMessage().GetViewOnce()
}

// handleUnavailableViewOnce records a view-once message that arrived on this
// linked device without its content. WhatsApp only lets the phone open view-once
// media, so a companion gets an <unavailable type="view_once"/> stub and the
// phone never resends it. Storing a "[view once]" placeholder (through the
// normal message path, webhook included) lets the MCP and the manager show that
// one arrived, like WhatsApp Web's "you can only open it on your phone".
// It reports whether v was such a stub.
func handleUnavailableViewOnce(client *whatsmeow.Client, messageStore *MessageStore, v *events.UndecryptableMessage, logger waLog.Logger) bool {
	if v == nil || !v.IsUnavailable || v.UnavailableType != events.UnavailableTypeViewOnce {
		return false
	}
	handleMessage(client, messageStore, &events.Message{
		Info:       v.Info,
		Message:    &waE2E.Message{Conversation: proto.String(viewOnceContent(""))},
		IsViewOnce: true,
	}, logger)
	return true
}
