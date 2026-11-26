package helpers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type listScheduledInput struct {
	ChatJID *string `json:"chat_jid,omitempty" jsonschema:"Optional. Only this chat (person like 60123456789@s.whatsapp.net or group like 123@g.us)"`
	Status  *string `json:"status,omitempty" jsonschema:"Optional. scheduled (default, still waiting), posted, cancelled, or all"`
}

type cancelScheduledInput struct {
	ChatJID   string `json:"chat_jid" jsonschema:"The chat JID the message was scheduled in (a phone number works for a person)"`
	MessageID string `json:"message_id" jsonschema:"ID of the scheduled message, from send_message or list_scheduled_messages"`
}

func listScheduledHandler(ctx context.Context, req *mcp.CallToolRequest, in listScheduledInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	if in.ChatJID != nil && strings.TrimSpace(*in.ChatJID) != "" {
		q.Set("chat_jid", chatJIDOrPhone(*in.ChatJID))
	}
	if in.Status != nil && strings.TrimSpace(*in.Status) != "" {
		status := strings.ToLower(strings.TrimSpace(*in.Status))
		switch status {
		case "scheduled", "posted", "cancelled", "all":
		default:
			return ErrResult("status must be scheduled, posted, cancelled or all"), nil, nil
		}
		q.Set("status", status)
	}
	path := "/scheduled"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	data, err := callAPI(http.MethodGet, path, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return ErrResult("failed to parse API response"), nil, nil
	}
	return &mcp.CallToolResult{}, resp, nil
}

func cancelScheduledHandler(ctx context.Context, req *mcp.CallToolRequest, in cancelScheduledInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.ChatJID) == "" || strings.TrimSpace(in.MessageID) == "" {
		return ErrResult("chat_jid and message_id are required"), nil, nil
	}
	if _, err := callAPI(http.MethodPost, "/unschedule", map[string]any{
		"recipient":  chatJIDOrPhone(in.ChatJID),
		"message_id": strings.TrimSpace(in.MessageID),
	}); err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	return OkResult(map[string]any{"success": true}), nil, nil
}
