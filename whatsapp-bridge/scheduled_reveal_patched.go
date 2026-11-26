//go:build grouphistory

package main

// The patched-whatsmeow half of scheduled_reveal.go. Every vimigo image builds
// the bridge with -tags grouphistory against patches/whatsmeow.patch, which
// adds events.MexUpdate (mex notifications whatsmeow does not handle) and
// MsgMetaInfo.Scheduled* (a scheduled message's reveal key and time from its
// stanza). A stock whatsmeow build leaves these hooks unset.

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func init() {
	scheduledMetaOf = func(m *events.Message) (string, []byte, time.Time) {
		mi := m.Info.MsgMetaInfo
		return mi.ScheduledRevealKeyID, mi.ScheduledRevealKey, mi.ScheduledTime
	}
	registerScheduledReveal = func(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
		client.AddEventHandler(func(evt interface{}) {
			if u, ok := evt.(*events.MexUpdate); ok {
				handleScheduledMex(client, store, u.Mex.OpName, u.Data, logger)
			}
		})
		go runIncomingRevealSweeper(context.Background(), client, store, logger)
	}
}
