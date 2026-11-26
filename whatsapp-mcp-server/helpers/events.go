package helpers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const sendEventDescription = "Send a WhatsApp event invitation (the Event card with Going / Not going / Maybe buttons) to a person or group. " +
	"Use it for meetings, launches, viewings or appointments where you want people to RSVP. " +
	"Times are RFC3339 with an offset (\"2026-11-28T15:00:00+08:00\") or a local \"2026-11-28 15:00\" in the bridge's timezone. " +
	"join_link is an existing call or meeting URL to put on the card; this tool cannot create a WhatsApp call link. " +
	"The answers show under the event in list_messages, and get_event_responses gives the full list with counts."

type sendEventInput struct {
	Recipient          string  `json:"recipient" jsonschema:"Phone number (no +) or group JID like 123@g.us to send the event to"`
	Name               string  `json:"name" jsonschema:"Event title shown on the card"`
	StartTime          string  `json:"start_time" jsonschema:"Start time: RFC3339 with offset, e.g. 2026-11-28T15:00:00+08:00, or local 2026-11-28 15:00"`
	EndTime            *string `json:"end_time,omitempty" jsonschema:"Optional end time, same formats as start_time"`
	Description        *string `json:"description,omitempty" jsonschema:"Optional details shown on the card"`
	Location           *string `json:"location,omitempty" jsonschema:"Optional place name, e.g. KL office, Level 3"`
	JoinLink           *string `json:"join_link,omitempty" jsonschema:"Optional existing call or meeting URL"`
	ExtraGuestsAllowed *bool   `json:"extra_guests_allowed,omitempty" jsonschema:"Optional. Let people say they are bringing extra guests"`
	NoDelay            *bool   `json:"no_delay,omitempty" jsonschema:"Optional. true sends instantly. Default false: WhatsApp shows typing for a random 3-30 s first."`
}

type getEventResponsesInput struct {
	MessageID string `json:"message_id" jsonschema:"The event's message ID, as shown in list_messages or returned by send_event"`
}

func sendEventHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in sendEventInput,
) (*mcp.CallToolResult, map[string]any, error) {
	fail := func(msg string) (*mcp.CallToolResult, map[string]any, error) {
		return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
			}, map[string]any{
				"success": false,
				"error":   msg,
			}, nil
	}
	if in.Recipient == "" || in.Name == "" || in.StartTime == "" {
		return fail("recipient, name and start_time are required")
	}
	payload := map[string]any{
		"no_delay":   in.NoDelay != nil && *in.NoDelay,
		"recipient":  in.Recipient,
		"name":       in.Name,
		"start_time": in.StartTime,
	}
	for key, v := range map[string]*string{
		"end_time":    in.EndTime,
		"description": in.Description,
		"location":    in.Location,
		"join_link":   in.JoinLink,
	} {
		if v != nil && *v != "" {
			payload[key] = *v
		}
	}
	if in.ExtraGuestsAllowed != nil {
		payload["extra_guests_allowed"] = *in.ExtraGuestsAllowed
	}

	data, err := callAPI(http.MethodPost, "/send-event", payload)
	if err != nil {
		return fail(err.Error())
	}
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return fail("failed to parse API response")
	}
	return &mcp.CallToolResult{}, resp, nil
}

func getEventResponsesHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in getEventResponsesInput,
) (*mcp.CallToolResult, any, error) {
	if in.MessageID == "" {
		return ErrResult("message_id is required"), nil, nil
	}
	data, err := callAPI(http.MethodGet, "/event-responses?message_id="+url.QueryEscape(in.MessageID), nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return ErrResult("failed to parse API response"), nil, nil
	}
	return &mcp.CallToolResult{}, resp, nil
}
