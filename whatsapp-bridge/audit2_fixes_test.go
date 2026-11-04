package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"whatsapp-bridge/auth"
	"whatsapp-bridge/config"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// realStore opens a store with the bridge's own schema: SQLite in a temp
// folder, or, with WA_TEST_PG_HOST set, a throwaway Postgres database
// "whatsapp" (user and password postgres) that is wiped first.
func realStore(t *testing.T) *MessageStore {
	t.Helper()
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	t.Cleanup(func() { isPostgres = prevPG })
	if host := os.Getenv("WA_TEST_PG_HOST"); host != "" {
		for k, v := range map[string]string{"IS_POSTGRES": "true", "POSTGRES_USER": "postgres", "POSTGRES_PASS": "postgres",
			"POSTGRES_HOST": host, "POSTGRES_PORT": "5432", "WHATSAPP_API_KEY": strings.Repeat("k", 40), "WHATSAPP_JWT_SECRET": strings.Repeat("s", 40)} {
			t.Setenv(k, v)
		}
		db, err := sql.Open("postgres", "postgresql://postgres:postgres@"+host+":5432/whatsapp?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })
	return ms
}

type storedRow struct {
	content         string
	deleted, edited bool
	sender          string
	fromMe          bool
	scanStatus      string
	present         bool
}

func readRow(t *testing.T, ms *MessageStore, id, chat string) storedRow {
	t.Helper()
	var r storedRow
	var status *string
	err := ms.db.QueryRow(pgPlaceholders(`SELECT content, deleted_at IS NOT NULL, edited_at IS NOT NULL, sender, is_from_me, scan_status
		FROM messages WHERE id = ? AND chat_jid = ?`), id, chat).Scan(&r.content, &r.deleted, &r.edited, &r.sender, &r.fromMe, &status)
	if err != nil {
		return storedRow{}
	}
	if status != nil {
		r.scanStatus = *status
	}
	r.present = true
	return r
}

// A history snapshot (pairing sync, /api/history-replay) carries the text a
// message had when the phone uploaded it. Storing it again must not bring back
// a message its sender revoked, nor undo an edit, and a revoke that arrived
// before its message must still hide it.
func TestReStoreKeepsRevokesAndEdits(t *testing.T) {
	ms := realStore(t)
	client := &whatsmeow.Client{Store: &wastore.Device{}}
	alice := types.NewJID("60111111111", types.DefaultUserServer)
	chat := alice.String()
	t0 := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(chat, "Alice", t0); err != nil {
		t.Fatal(err)
	}
	store := func(id, content string) {
		t.Helper()
		if err := ms.StoreMessage(id, chat, "60111111111", content, t0, false, "", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	store("M1", "SENT-BY-MISTAKE")
	store("M2", "original typo")
	handleMessage(client, ms, protocolEvent(alice, alice, false, "P1", revokeOf("M1"), t0.Add(time.Minute)), waLog.Noop)
	handleMessage(client, ms, protocolEvent(alice, alice, false, "P2", editOf("M2", "corrected text"), t0.Add(2*time.Minute)), waLog.Noop)
	handleMessage(client, ms, protocolEvent(alice, alice, false, "P3", revokeOf("M3"), t0.Add(3*time.Minute)), waLog.Noop)

	// The stale snapshot arrives.
	store("M1", "SENT-BY-MISTAKE")
	store("M2", "original typo")
	store("M3", "REVOKED-BEFORE-IT-WAS-STORED")

	if r := readRow(t, ms, "M1", chat); !r.deleted {
		t.Errorf("M1 revoke undone by the re-store: %+v", r)
	}
	if r := readRow(t, ms, "M2", chat); !r.edited || r.content != "corrected text (edited)" {
		t.Errorf("M2 edit undone by the re-store: %+v", r)
	}
	if r := readRow(t, ms, "M3", chat); !r.deleted {
		t.Errorf("M3 revoke that arrived first was lost: %+v", r)
	}

	// A revoke from someone else is not kept for later.
	bob := types.NewJID("60122222222", types.DefaultUserServer)
	group := types.NewJID("120363000000000009", types.GroupServer)
	if err := ms.StoreChat(group.String(), "Team", t0); err != nil {
		t.Fatal(err)
	}
	handleMessage(client, ms, protocolEvent(group, bob, false, "P4", revokeOf("ALICEG"), t0), waLog.Noop)
	if err := ms.StoreMessage("ALICEG", group.String(), "60111111111", "alice in the group", t0, false, "", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if r := readRow(t, ms, "ALICEG", group.String()); r.deleted {
		t.Errorf("another member's early revoke deleted Alice's message: %+v", r)
	}

	// Another member revoking first does not block the author's own revoke.
	handleMessage(client, ms, protocolEvent(group, bob, false, "P5", revokeOf("ALICEG2"), t0), waLog.Noop)
	handleMessage(client, ms, protocolEvent(group, alice, false, "P6", revokeOf("ALICEG2"), t0.Add(time.Second)), waLog.Noop)
	if err := ms.StoreMessage("ALICEG2", group.String(), "60111111111", "alice deleted this", t0, false, "", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if r := readRow(t, ms, "ALICEG2", group.String()); !r.deleted {
		t.Errorf("the author's early revoke was lost behind another member's: %+v", r)
	}
}

// The startup LID merge joins the @lid and phone-number copies of a chat. A
// row and a same-ID row in the other chat are only the same message when the
// author matches; otherwise neither may change or delete the other.
func TestLIDMergeKeepsOtherAuthorsRowsApart(t *testing.T) {
	ms := realStore(t)
	lid, pn := "111122223333444@lid", "60122222222@s.whatsapp.net"
	t0 := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	for _, c := range []string{lid, pn} {
		if err := ms.StoreChat(c, "Contact", t0); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(id, chat, sender, content string, fromMe bool, deleted, edited bool) {
		t.Helper()
		var del, ed any
		if deleted {
			del = t0
		}
		if edited {
			ed = t0.Add(time.Hour)
		}
		if _, err := ms.db.Exec(pgPlaceholders(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at, edited_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), id, chat, sender, content, t0, fromMe, del, ed); err != nil {
			t.Fatal(err)
		}
	}
	// The owner's messages in the phone chat; the contact reused their IDs in
	// its @lid chat and edited or revoked its own copies.
	ins("A", pn, "60199999999", "owner says no", true, false, false)
	ins("A", lid, "111122223333444", "owner says yes (edited)", false, false, true)
	ins("B", pn, "60199999999", "owner message B", true, false, false)
	ins("B", lid, "111122223333444", "x", false, true, false)
	// The other direction: the owner's row exists only under @lid, and the
	// contact used that ID in the phone chat.
	ins("C", lid, "60199999999", "owner lid-era message", true, false, false)
	ins("C", pn, "60122222222", "contact text", false, false, false)
	// A true duplicate (same author in both chats) still merges.
	ins("D", pn, "60122222222", "hello", false, false, false)
	ins("D", lid, "111122223333444", "hello (edited)", false, false, true)

	tx, err := ms.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := moveLIDChatRows(tx, isPostgres, lid, pn); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if r := readRow(t, ms, "A", pn); r.content != "owner says no" || r.edited {
		t.Errorf("owner's A took the contact's edit: %+v", r)
	}
	if r := readRow(t, ms, "B", pn); r.deleted {
		t.Errorf("owner's B took the contact's revoke: %+v", r)
	}
	if r := readRow(t, ms, "C", lid); !r.present || !r.fromMe {
		t.Errorf("owner's lid-only C was deleted: %+v", r)
	}
	if r := readRow(t, ms, "D", pn); r.content != "hello (edited)" || !r.edited {
		t.Errorf("same-author duplicate D did not merge the edit: %+v", r)
	}
	if r := readRow(t, ms, "D", lid); r.present {
		t.Errorf("same-author duplicate D left in the @lid chat: %+v", r)
	}
}

// A file verdict covers the file's name, so copies of the same bytes under a
// risky name keep their own warning instead of taking a clean verdict.
func TestSharedVerdictKeepsFileNameWarnings(t *testing.T) {
	ms := realStore(t)
	chat := "60111111111@s.whatsapp.net"
	if err := ms.StoreChat(chat, "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	data := []byte("var s=new ActiveXObject('WScript.Shell');s.Run('calc.exe');\n")
	sum := sha256.Sum256(data)
	doc := func(id, name string) {
		t.Helper()
		if err := ms.StoreMessage(id, chat, "60111111111", "", time.Now(), false, "document", name, "", "", nil, sum[:], nil, uint64(len(data)), "", "", "", false); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join("store", chat)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	// Order A: the benign copy is checked first.
	doc("A1", "notes.txt")
	if _, _, _, _, s, err := downloadMedia(nil, ms, "A1", chat); err != nil || s.Status != "clean" {
		t.Fatalf("notes.txt: %v %+v", err, s)
	}
	doc("A2", "invoice.pdf.js")
	if s, _ := ms.GetFileScan("A2", chat); s.Status == "clean" {
		t.Errorf("invoice.pdf.js took the clean verdict of notes.txt: %+v", s)
	}
	// Order B: the risky copy arrives first, then the benign one is checked.
	doc("B1", "payment.js")
	doc("B2", "readme.txt")
	if _, _, _, _, _, err := downloadMedia(nil, ms, "B2", chat); err != nil {
		t.Fatal(err)
	}
	if s, _ := ms.GetFileScan("B1", chat); s.Status == "clean" {
		t.Errorf("payment.js was turned clean by readme.txt: %+v", s)
	}
}

// Once a message is deleted for everyone, its media is not served and replies
// that quoted it no longer show its text.
func TestRevokedMediaAndQuotesAreNotServed(t *testing.T) {
	ms := realStore(t)
	chat := "60100000009@s.whatsapp.net"
	t0 := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(chat, "DummyV", t0); err != nil {
		t.Fatal(err)
	}
	img := []byte("V01-DUMMY-IMAGE")
	sum := sha256.Sum256(img)
	if err := ms.StoreMessage("IMG", chat, "60100000009", "", t0, false, "image", "v.jpg", "", "", make([]byte, 32), sum[:], nil, uint64(len(img)), "", "", "", false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("store", chat)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "v.jpg"), img, 0o644)
	if err := ms.StoreMessage("TXT", chat, "60100000009", "V01-RETRACTED", t0.Add(time.Minute), false, "", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("REP", chat, "60100000000", "a reply", t0.Add(2*time.Minute), true, "", "", "", "", nil, nil, nil, 0, "TXT", "60100000009", "V01-RETRACTED", false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"IMG", "TXT"} {
		if err := ms.DeleteMessage(chat, id); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _, _, _, _, err := downloadMedia(nil, ms, "IMG", chat); ok || err == nil {
		t.Errorf("revoked image still served: ok=%v err=%v", ok, err)
	}
	list, err := ms.ListMessages(ListMessagesParams{ChatJid: &chat, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "V01-RETRACTED") {
		t.Errorf("list_messages still shows the revoked text:\n%s", list)
	}
	if src, err := ms.GetQuoteSource("TXT", chat); err == nil {
		t.Errorf("a revoked message can still be quoted: %+v", src)
	}
}

// A request rejected before authentication is answered and closed at once,
// even when it announced a body it never sends.
func TestPreAuthRejectionDoesNotWaitForTheBody(t *testing.T) {
	cfg := &config.Config{APIKey: strings.Repeat("k", 40), JWTSecret: []byte(strings.Repeat("s", 40))}
	mux := http.NewServeMux()
	mux.Handle("/api/", auth.JwtAuthMiddleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	mux.Handle("/auth/login", auth.LoginHandler(cfg))
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ReadHeaderTimeout = 10 * time.Second
	srv.Config.IdleTimeout = 2 * time.Minute
	srv.Start()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, req := range []string{
		"POST /api/send HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n",
		"POST /api/send HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n",
		"POST /auth/login HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer wrong\r\nContent-Length: 100\r\n\r\n",
	} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(req))
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if err != nil || !strings.Contains(line, " 401 ") {
			t.Errorf("%q: got %q, %v; want an immediate 401", strings.SplitN(req, "\r\n", 2)[0], line, err)
		}
	}
}

// The object walk stops when the scan deadline has passed and says so, so a
// PDF built to expand every object stream cannot keep a CPU busy.
func TestPDFWalkStopsAtTheDeadline(t *testing.T) {
	data := buildPDFObjStm("<< /Type /Catalog /Pages 2 0 R >>", pdfPages, pdfPage)
	ctx, err := readPDF(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, limited := pdfObjectReasons(ctx, time.Now().Add(-time.Second)); !limited {
		t.Error("walk past its deadline did not report limited")
	}
	if _, limited := pdfObjectReasons(ctx, time.Now().Add(time.Minute)); limited {
		t.Error("walk with time left reported limited")
	}
}

// A webhook receiver that accepts but never answers is cut off by the client
// timeout instead of holding the delivery forever.
func TestWebhookDeliveryIsTimeBounded(t *testing.T) {
	if webhookClient.Timeout <= 0 || webhookClient.Timeout > time.Minute {
		t.Fatalf("webhook client timeout %v, want a bound", webhookClient.Timeout)
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	prev, prevBackoff := webhookClient, webhookBackoff
	webhookClient, webhookBackoff = &http.Client{Timeout: 200 * time.Millisecond}, nil
	defer func() { webhookClient, webhookBackoff = prev, prevBackoff }()
	start := time.Now()
	if err := postWebhook(srv.URL, []byte(`{}`)); err == nil {
		t.Error("stalled receiver: no error")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("delivery took %v", time.Since(start))
	}
}

// A failed or 5xx delivery is retried, so a receiver that is briefly down
// still gets the message.
func TestWebhookDeliveryRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	prevBackoff := webhookBackoff
	webhookBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { webhookBackoff = prevBackoff }()
	if err := postWebhook(srv.URL, []byte("{}")); err != nil || calls != 3 {
		t.Errorf("err=%v calls=%d, want delivered on the third try", err, calls)
	}
}
