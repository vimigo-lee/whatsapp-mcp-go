package helpers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type stubCall struct {
	method, path, query string
	body                map[string]any
}

// stubBridgeCalls is stubBridge that also records method, path and query.
func stubBridgeCalls(t *testing.T) *[]stubCall {
	t.Helper()
	var got []stubCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := stubCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		_ = json.Unmarshal(b, &c.body)
		got = append(got, c)
		w.Write([]byte(`{"success":true,"scheduled":[]}`))
	}))
	t.Cleanup(srv.Close)
	prevBase := apiBaseURL
	apiBaseURL = srv.URL + "/api"
	tokenMutex.Lock()
	prevTok, prevExp := jwtToken, tokenExpiresAt
	jwtToken, tokenExpiresAt = "test", time.Now().Add(time.Hour)
	tokenMutex.Unlock()
	t.Cleanup(func() {
		apiBaseURL = prevBase
		tokenMutex.Lock()
		jwtToken, tokenExpiresAt = prevTok, prevExp
		tokenMutex.Unlock()
	})
	return &got
}

func TestSendMessageForwardsScheduleAt(t *testing.T) {
	calls := stubBridge(t)
	at := " 2026-11-28T15:00:00+08:00 "
	res, _, _ := sendMessageHandler(context.Background(), nil, sendMessageInput{Recipient: "60123", Message: "hi", ScheduleAt: &at})
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("isError=%v calls=%d", res.IsError, len(*calls))
	}
	if (*calls)[0]["schedule_at"] != "2026-11-28T15:00:00+08:00" {
		t.Errorf("schedule_at = %v", (*calls)[0]["schedule_at"])
	}

	*calls = nil
	sendMessageHandler(context.Background(), nil, sendMessageInput{Recipient: "60123", Message: "hi"})
	if _, has := (*calls)[0]["schedule_at"]; has {
		t.Error("schedule_at sent when not requested")
	}
}

func TestListScheduledMessages(t *testing.T) {
	calls := stubBridgeCalls(t)
	chat, status := "+60123", "all"
	res, _, _ := listScheduledHandler(context.Background(), nil, listScheduledInput{ChatJID: &chat, Status: &status})
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("isError=%v calls=%d", res.IsError, len(*calls))
	}
	c := (*calls)[0]
	if c.method != http.MethodGet || c.path != "/api/scheduled" || c.query != "chat_jid=60123%40s.whatsapp.net&status=all" {
		t.Errorf("call = %+v", c)
	}

	bad := "pending"
	if res, _, _ := listScheduledHandler(context.Background(), nil, listScheduledInput{Status: &bad}); !res.IsError {
		t.Error("unknown status accepted")
	}
	if len(*calls) != 1 {
		t.Error("bridge called for a rejected status")
	}
}

func TestCancelScheduledMessage(t *testing.T) {
	calls := stubBridgeCalls(t)
	res, _, _ := cancelScheduledHandler(context.Background(), nil, cancelScheduledInput{ChatJID: "60123", MessageID: " 3EB0AA "})
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("isError=%v calls=%d", res.IsError, len(*calls))
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/api/unschedule" ||
		c.body["recipient"] != "60123@s.whatsapp.net" || c.body["message_id"] != "3EB0AA" {
		t.Errorf("call = %+v", c)
	}
	if res, _, _ := cancelScheduledHandler(context.Background(), nil, cancelScheduledInput{ChatJID: "60123"}); !res.IsError {
		t.Error("missing message_id accepted")
	}
}
