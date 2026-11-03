package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// An inline payload is used as the history itself, so accepting one let an API
// caller write any chats and messages, including chat JIDs that walk out of
// the media folder. Replay only accepts notifications that point at WhatsApp.
func TestHistoryReplayRejectsInlinePayloads(t *testing.T) {
	h := handleHistoryReplay(&whatsmeow.Client{Store: &wastore.Device{}}, nil)
	for name, notif := range map[string]string{
		"inline payload": `{"syncType":"RECENT","initialHistBootstrapInlinePayload":"eJwDAAAAAAE="}`,
		"no direct path": `{"syncType":"RECENT","mediaKey":"AQ=="}`,
	} {
		body := `{"notifications":[{"id":"N1","notification":` + notif + `}]}`
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/history-replay", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
}

func TestStorableChatJID(t *testing.T) {
	for jid, want := range map[string]bool{
		"60123456789@s.whatsapp.net":    true,
		"120363000000000001@g.us":       true,
		"123456789012345@lid":           true,
		"status@broadcast":              true,
		"120363000000000002@newsletter": true,
		"x@/../../elsewhere/data":       false,
		"..@s.whatsapp.net":             false,
		"a/b@s.whatsapp.net":            false,
	} {
		j, err := types.ParseJID(jid)
		if err != nil {
			if want {
				t.Errorf("%s: parse error %v", jid, err)
			}
			continue
		}
		if got := storableChatJID(j); got != want {
			t.Errorf("storableChatJID(%q) = %v, want %v", jid, got, want)
		}
	}
}

// Whatever chat JID a row carries, media is never read from or written to a
// folder outside store/.
func TestFetchMediaStaysInsideTheStore(t *testing.T) {
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	t.Chdir(t.TempDir())
	s := mediaReuseStore(t)
	chat := "x@/../../elsewhere/data"
	if err := s.StoreMessage("M1", chat, "x", "", time.Now(), false, "document", "secret.txt", "", "", []byte{1}, nil, []byte{1}, 1, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, _, _, err := downloadMedia(nil, s, "M1", chat); ok || err == nil {
		t.Fatalf("download for a traversal chat JID: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat("elsewhere"); !os.IsNotExist(err) {
		t.Error("a folder outside store/ was created")
	}
}
