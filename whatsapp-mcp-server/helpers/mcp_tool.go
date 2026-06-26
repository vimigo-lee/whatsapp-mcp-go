package helpers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// InitMcpTool initializes MCP tool for the MCP server
func InitMcpTool() {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "whatsapp-mcp",
		Version: "v1.0.0",
	}, nil)

	mcp.AddTool[searchContactsInput, any](server, &mcp.Tool{
		Name:        "search_contacts",
		Description: "Search WhatsApp contacts by name or phone number.",
	}, searchContactsHandler)

	mcp.AddTool[listMessagesInput, any](server, &mcp.Tool{
		Name:        "list_messages",
		Description: "Get WhatsApp messages matching specified criteria with optional context around matches.",
	}, listMessagesHandler)

	mcp.AddTool[getMessageContextInput, any](server, &mcp.Tool{
		Name:        "get_message_context",
		Description: "Get surrounding messages (context) around a specific WhatsApp message.",
	}, getMessageContextHandler)

	mcp.AddTool[listChatsInput, any](server, &mcp.Tool{
		Name:        "list_chats",
		Description: "Get list of WhatsApp chats, optionally filtered and sorted.",
	}, listChatsHandler)

	mcp.AddTool[getChatInput, any](server, &mcp.Tool{
		Name:        "get_chat",
		Description: "Get metadata of a specific WhatsApp chat by JID.",
	}, getChatHandler)

	mcp.AddTool[getDirectChatByContactInput, any](server, &mcp.Tool{
		Name:        "get_direct_chat_by_contact",
		Description: "Get direct (1:1) chat metadata by contact phone number.",
	}, getDirectChatByContactHandler)

	mcp.AddTool[getContactChatsInput, any](server, &mcp.Tool{
		Name:        "get_contact_chats",
		Description: "Get all chats involving a specific contact (JID).",
	}, getContactChatsHandler)

	mcp.AddTool[getLastInteractionInput, any](server, &mcp.Tool{
		Name:        "get_last_interaction",
		Description: "Get most recent WhatsApp message involving the contact.",
	}, getLastInteractionHandler)

	mcp.AddTool[sendMessageInput, map[string]any](server, &mcp.Tool{
		Name:        "send_message",
		Description: "Send a TEXT message to a person or group on WhatsApp. For groups use the group JID. Text only — to send an image, video, document or any file use send_media instead (and upload_media first if the file is local), not this tool.",
	}, sendMessageHandler)

	mcp.AddTool[sendMediaInput, map[string]any](server, &mcp.Tool{
		Name:        "send_media",
		Description: "Send an image, video or document (with an optional text caption) to a WhatsApp chat. Provide the media as EXACTLY ONE of: 'url' (a public http(s) link — preferred; the server downloads it), 'base64' (raw media bytes), or 'media_path' (a path to a file ON THE SERVER — rare). This server is REMOTE and CANNOT read the user's local machine, so do NOT pass a local/desktop path as media_path; if the user has a LOCAL file, first call upload_media to upload it and pass the returned 'url' here.",
	}, sendMediaHandler)

	mcp.AddTool[uploadMediaInput, map[string]any](server, &mcp.Tool{
		Name:        "upload_media",
		Description: "Upload a LOCAL file to temporary storage (MinIO) so it can be sent with send_media — this server is REMOTE and cannot read the user's disk. Returns a short-lived 'url' for send_media; auto-deleted after 1 day. DEFAULT: omit 'base64' to get an upload slot {put_url, get_url}, PUT the raw file bytes to put_url (no special headers), then call send_media with url=get_url. Only pass 'base64' (raw bytes, no data: prefix) when explicitly asked — the server then uploads it for you and returns {url}.",
	}, uploadMediaHandler)

	mcp.AddTool[sendAudioMessageInput, map[string]any](server, &mcp.Tool{
		Name:        "send_audio_message",
		Description: "Send audio/voice message (converted to Opus .ogg if needed).",
	}, sendAudioMessageHandler)

	mcp.AddTool[downloadMediaInput, map[string]any](server, &mcp.Tool{
		Name:        "download_media",
		Description: "Download media from a WhatsApp message and return local file path.",
	}, downloadMediaHandler)

	mcp.AddTool[getLoginStatusInput, any](server, &mcp.Tool{
		Name:        "get_login_status",
		Description: "Check whether the WhatsApp bridge is connected and logged in. Returns {connected, logged_in, pairing_required}.",
	}, getLoginStatusHandler)

	mcp.AddTool[getPairingQrInput, any](server, &mcp.Tool{
		Name:        "get_pairing_qr",
		Description: "Fetch the WhatsApp pairing QR as a PNG image. Returns image content when pairing is required; returns a text message when the bridge is already logged in or pairing has not started.",
	}, getPairingQrHandler)

	// ── Group management ────────────────────────────────────────────────────
	mcp.AddTool[createGroupInput, any](server, &mcp.Tool{
		Name:        "create_group",
		Description: "Create a new WhatsApp group with the given name and initial participants (phone numbers, no +). The current number becomes the group owner. Returns the new group's JID and roster.",
	}, createGroupHandler)

	mcp.AddTool[listGroupsInput, any](server, &mcp.Tool{
		Name:        "list_groups",
		Description: "List every WhatsApp group this number is a member of, with JID, name and participants.",
	}, listGroupsHandler)

	mcp.AddTool[addGroupParticipantsInput, any](server, &mcp.Tool{
		Name:        "add_group_participants",
		Description: "Add one or more participants (phone numbers, no +) to a WhatsApp group. Returns a per-participant result; an add can fail if the person's privacy settings block direct adds (then 'needs_invite' is set — share the invite link instead). Set share_history=true to silently share recent group messages with the members that were added (so their chat is not empty); share_history_count sets an exact number (1-100) instead of the full recent window.",
	}, addGroupParticipantsHandler)

	mcp.AddTool[groupParticipantsInput, any](server, &mcp.Tool{
		Name:        "remove_group_participants",
		Description: "Remove (kick) one or more participants from a WhatsApp group. Requires this number to be a group admin.",
	}, removeGroupParticipantsHandler)

	mcp.AddTool[groupParticipantsInput, any](server, &mcp.Tool{
		Name:        "promote_group_admins",
		Description: "Promote one or more group participants to admin. Requires this number to be a group admin.",
	}, promoteGroupAdminsHandler)

	mcp.AddTool[groupParticipantsInput, any](server, &mcp.Tool{
		Name:        "demote_group_admins",
		Description: "Demote one or more group admins back to regular members. Requires this number to be a group admin.",
	}, demoteGroupAdminsHandler)

	mcp.AddTool[setGroupNameInput, any](server, &mcp.Tool{
		Name:        "set_group_name",
		Description: "Change a WhatsApp group's name (max 25 characters). Requires admin if the group is locked.",
	}, setGroupNameHandler)

	mcp.AddTool[setGroupTopicInput, any](server, &mcp.Tool{
		Name:        "set_group_topic",
		Description: "Change a WhatsApp group's topic/description. Requires admin if the group is locked.",
	}, setGroupTopicHandler)

	mcp.AddTool[groupInviteLinkInput, any](server, &mcp.Tool{
		Name:        "get_group_invite_link",
		Description: "Get a WhatsApp group's invite link. Set reset=true to revoke the current link and generate a new one. Requires this number to be a group admin.",
	}, getGroupInviteLinkHandler)

	mcp.AddTool[leaveGroupInput, any](server, &mcp.Tool{
		Name:        "leave_group",
		Description: "Leave a WhatsApp group this number is a member of.",
	}, leaveGroupHandler)

	isHttp := strings.ToLower(ReadEnv("IS_HTTP", "false")) == "true" ||
		strings.ToLower(ReadEnv("IS_HTTP", "0")) == "1"

	ctx := context.Background()

	if isHttp {
		addr := ReadEnv("HTTP_BASE_URL", "0.0.0.0:5777")
		slog.Info("Starting WhatsApp MCP HTTP streaming", "addr", addr)

		handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
			return server
		}, nil)
		if err := http.ListenAndServe(addr, handler); err != nil {
			log.Fatalf("Server failed: %v", err)
		}
	} else {
		slog.Info("Starting WhatsApp MCP server in stdio mode")
		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Fatalf("stdio failed: %v", err)
		}
	}
}

type searchContactsInput struct {
	Query string `json:"query" mcp:"description:Search term to match against contact names or phone numbers"`
}

type listMessagesInput struct {
	After             *string `mcp:"description:ISO-8601 formatted string"`
	Before            *string `json:"before,omitempty" jsonschema:"description:ISO-8601 formatted string"`
	SenderPhoneNumber *string `json:"sender_phone_number,omitempty"`
	ChatJid           *string `json:"chat_jid,omitempty"`
	Query             *string `json:"query,omitempty" jsonschema:"description:Search term in message content"`
	Limit             int     `json:"limit" jsonschema:"default:20"`
	Page              int     `json:"page" jsonschema:"default:0"`
	IncludeContext    bool    `json:"include_context" jsonschema:"default:true"`
	ContextBefore     int     `json:"context_before" jsonschema:"default:1"`
	ContextAfter      int     `json:"context_after" jsonschema:"default:1"`
}

type getMessageContextInput struct {
	MessageID string `json:"message_id" jsonschema:"description:The ID of the message"`
	Before    int    `json:"before" jsonschema:"default:5"`
	After     int    `json:"after" jsonschema:"default:5"`
}

type listChatsInput struct {
	Query              *string `json:"query,omitempty"`
	Limit              int     `json:"limit" jsonschema:"default:20"`
	Page               int     `json:"page" jsonschema:"default:0"`
	IncludeLastMessage bool    `json:"include_last_message" jsonschema:"default:true"`
	SortBy             string  `json:"sort_by" jsonschema:"default:last_active,enum:last_active|name"`
}

type getChatInput struct {
	ChatJid            string `json:"chat_jid" jsonschema:"description:The JID of the chat"`
	IncludeLastMessage bool   `json:"include_last_message" jsonschema:"default:true"`
}

type getDirectChatByContactInput struct {
	SenderPhoneNumber string `json:"sender_phone_number" jsonschema:"description:Phone number with country code"`
}

type getContactChatsInput struct {
	Jid   string `json:"jid"`
	Limit int    `json:"limit" jsonschema:"default:20"`
	Page  int    `json:"page" jsonschema:"default:0"`
}

type getLastInteractionInput struct {
	Jid string `json:"jid" jsonschema:"description:The contact's JID"`
}

type sendMessageInput struct {
	Recipient string `json:"recipient" jsonschema:"description:Phone number (no +) or group JID like 123@g.us"`
	Message   string `json:"message"`
}

type sendMediaInput struct {
	Recipient string  `json:"recipient" jsonschema:"description:Phone number (no +) or group JID like 123@g.us"`
	URL       *string `json:"url,omitempty" jsonschema:"description:Public http(s) URL of the media; the server downloads it. Provide exactly one of url/base64/media_path."`
	Base64    *string `json:"base64,omitempty" jsonschema:"description:Raw base64 of the media bytes (no data: prefix). Provide exactly one of url/base64/media_path."`
	MediaPath *string `json:"media_path,omitempty" jsonschema:"description:Absolute path to a file ON THE SERVER (not the user's local machine) — rare. Provide exactly one of url/base64/media_path."`
	Filename  *string `json:"filename,omitempty" jsonschema:"description:Filename with extension (e.g. photo.jpg); drives the media type. Optional when url already ends in a filename."`
	Caption   *string `json:"caption,omitempty" jsonschema:"description:Optional text caption sent with the media"`
}

type sendAudioMessageInput struct {
	Recipient string `json:"recipient"`
	MediaPath string `json:"media_path" jsonschema:"description:Absolute path to audio file"`
}

type downloadMediaInput struct {
	MessageID string `json:"message_id"`
	ChatJid   string `json:"chat_jid"`
}

type getLoginStatusInput struct{}

type getPairingQrInput struct{}

type createGroupInput struct {
	Name         string   `json:"name" jsonschema:"description:Group name (max 25 characters)"`
	Participants []string `json:"participants" jsonschema:"description:Phone numbers (no +) to add as initial members; your own number is added automatically"`
}

type listGroupsInput struct{}

// Shared by remove/promote/demote — same shape, different action. (add has its
// own input with extra history-sharing fields.)
type groupParticipantsInput struct {
	Jid          string   `json:"jid" jsonschema:"description:The group JID like 123@g.us"`
	Participants []string `json:"participants" jsonschema:"description:Phone numbers (no +) or member JIDs to act on"`
}

// add_group_participants extends the shared shape with optional silent
// history-sharing for the members that get added.
type addGroupParticipantsInput struct {
	Jid               string   `json:"jid" jsonschema:"description:The group JID like 123@g.us"`
	Participants      []string `json:"participants" jsonschema:"description:Phone numbers (no +) or member JIDs to add"`
	ShareHistory      bool     `json:"share_history,omitempty" jsonschema:"description:When true, silently share recent group messages with members that were successfully added so their chat is not empty. Shares as many recent messages as exist, up to WhatsApp's cap of 100. Members that need an invite do not receive it."`
	ShareHistoryCount int      `json:"share_history_count,omitempty" jsonschema:"description:Exact number of newest messages to share (1-100). Overrides share_history's full-window default and implies sharing is on."`
}

type setGroupNameInput struct {
	Jid  string `json:"jid" jsonschema:"description:The group JID like 123@g.us"`
	Name string `json:"name" jsonschema:"description:New group name (max 25 characters)"`
}

type setGroupTopicInput struct {
	Jid   string `json:"jid" jsonschema:"description:The group JID like 123@g.us"`
	Topic string `json:"topic" jsonschema:"description:New group topic/description"`
}

type groupInviteLinkInput struct {
	Jid   string `json:"jid" jsonschema:"description:The group JID like 123@g.us"`
	Reset bool   `json:"reset" jsonschema:"description:Revoke the current link and generate a new one,default:false"`
}

type leaveGroupInput struct {
	Jid string `json:"jid" jsonschema:"description:The group JID like 123@g.us"`
}

func callAPI(method, path string, body any) ([]byte, error) {
	token, err := GetOrRefreshJwtToken()
	if err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(b)
	}

	fullURL := fmt.Sprintf("%s%s", apiBaseURL, path)
	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{Timeout: apiTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}

func sendMessageHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in sendMessageInput,
) (*mcp.CallToolResult, map[string]any, error) {
	if in.Recipient == "" || in.Message == "" {
		return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: "recipient and message are required"},
				},
			}, map[string]any{
				"success": false,
				"error":   "recipient and message are required",
			}, nil
	}

	payload := map[string]any{
		"recipient": in.Recipient,
		"message":   in.Message,
		"no_delay":  true,
	}

	data, err := callAPI(http.MethodPost, "/send", payload)
	if err != nil {
		return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: err.Error()},
				},
			}, map[string]any{
				"success": false,
				"error":   err.Error(),
			}, nil
	}

	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: "failed to parse API response"},
				},
			}, map[string]any{
				"success": false,
				"error":   "failed to parse API response",
			}, nil
	}

	return &mcp.CallToolResult{}, resp, nil
}

func searchContactsHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in searchContactsInput,
) (*mcp.CallToolResult, any, error) {
	if in.Query == "" {
		return ErrResult("query is required"), nil, nil
	}

	data, err := callAPI(http.MethodGet, "/contacts/search?q="+in.Query, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	var result struct {
		Contacts []map[string]any `json:"contacts"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid response format"), nil, nil
	}

	return OkResult(result.Contacts), nil, nil
}

// list_messages (similar for all read/list tools that return slices/maps)
func listMessagesHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in listMessagesInput,
) (*mcp.CallToolResult, any, error) {
	q := ""
	if in.After != nil {
		q += "&after=" + *in.After
	}
	if in.Before != nil {
		q += "&before=" + *in.Before
	}
	if in.SenderPhoneNumber != nil {
		q += "&sender=" + *in.SenderPhoneNumber
	}
	if in.ChatJid != nil {
		q += "&chat=" + *in.ChatJid
	}
	if in.Query != nil {
		q += "&search=" + *in.Query
	}
	q += fmt.Sprintf("&limit=%d&page=%d", in.Limit, in.Page)
	if in.IncludeContext {
		q += "&context=true"
	}

	data, err := callAPI(http.MethodGet, "/messages?"+strings.TrimPrefix(q, "&"), nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	// The /messages endpoint returns plain text → we return it as string
	return OkResult(string(data)), nil, nil
}

// download_media (same idea)
func downloadMediaHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in downloadMediaInput,
) (*mcp.CallToolResult, map[string]any, error) {
	path, err := DownloadMedia(in.MessageID, in.ChatJid)
	if err != nil || path == "" {
		msg := "failed to download media"
		if err != nil {
			msg += ": " + err.Error()
		}
		return &mcp.CallToolResult{}, map[string]any{
			"success": false,
			"message": msg,
		}, nil
	}
	return &mcp.CallToolResult{}, map[string]any{
		"success":   true,
		"message":   "Media downloaded successfully",
		"file_path": path,
	}, nil
}

func getMessageContextHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in getMessageContextInput,
) (*mcp.CallToolResult, any, error) {
	if in.MessageID == "" {
		return ErrResult("message_id is required"), nil, nil
	}

	path := fmt.Sprintf("/messages/context/%s?before=%d&after=%d",
		in.MessageID, in.Before, in.After)

	data, err := callAPI(http.MethodGet, path, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	var ctxData map[string]any
	if err := json.Unmarshal(data, &ctxData); err != nil {
		return ErrResult("invalid context response"), nil, nil
	}

	return OkResult(ctxData), nil, nil
}

func listChatsHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in listChatsInput,
) (*mcp.CallToolResult, any, error) {
	q := fmt.Sprintf("?limit=%d&page=%d", in.Limit, in.Page)
	if in.Query != nil && *in.Query != "" {
		q += "&q=" + *in.Query
	}
	if in.SortBy != "" {
		q += "&sort=" + in.SortBy
	}

	data, err := callAPI(http.MethodGet, "/chats"+q, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	var result struct {
		Chats []map[string]any `json:"chats"`
		Count int              `json:"count"`
	}
	_ = json.Unmarshal(data, &result) // best effort

	return OkResult(result.Chats), nil, nil
}

func getChatHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in getChatInput,
) (*mcp.CallToolResult, any, error) {
	if in.ChatJid == "" {
		return ErrResult("chat_jid is required"), nil, nil
	}

	data, err := callAPI(http.MethodGet, "/chats/"+in.ChatJid, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid chat response"), nil, nil
	}

	return OkResult(result["chat"]), nil, nil
}

func getDirectChatByContactHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in getDirectChatByContactInput,
) (*mcp.CallToolResult, any, error) {
	if in.SenderPhoneNumber == "" {
		return ErrResult("sender_phone_number is required"), nil, nil
	}

	// GET /api/direct-contacts/{phone}/chat
	path := fmt.Sprintf("/direct-contacts/%s/chat", in.SenderPhoneNumber)

	data, err := callAPI(http.MethodGet, path, nil)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{
					&mcp.TextContent{Text: "No direct (1:1) chat found for this phone number"},
				},
			}, nil, nil
		}
		return ErrResult(err.Error()), nil, nil
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("failed to parse chat response"), nil, nil
	}

	chat, ok := result["chat"]
	if !ok {
		return ErrResult("chat object missing in response"), nil, nil
	}

	return &mcp.CallToolResult{}, chat, nil
}

func getContactChatsHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in getContactChatsInput,
) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	page := in.Page
	if page < 0 {
		page = 0
	}

	// GET /api/contacts/{jid}/chats?limit=...&page=...
	path := fmt.Sprintf("/contacts/%s/chats?limit=%d&page=%d", in.Jid, limit, page)

	data, err := callAPI(http.MethodGet, path, nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	var result struct {
		Chats []map[string]any `json:"chats"`
		Count int              `json:"count"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("failed to parse chats response"), nil, nil
	}

	return &mcp.CallToolResult{}, result.Chats, nil
}

func getLastInteractionHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in getLastInteractionInput,
) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}

	// We simulate it by asking for 1 message from that sender
	data, err := callAPI(http.MethodGet, "/messages?sender="+in.Jid+"&limit=1", nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}

	return OkResult(string(data)), nil, nil
}

// maxMediaBytes bounds how much a remote URL can stream into memory (WhatsApp
// caps documents ~100 MB anyway).
const maxMediaBytes = 100 * 1024 * 1024

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// sendMediaHandler sends media (with an optional caption) to the bridge /send.
// The media comes from EXACTLY ONE of: a public URL (downloaded server-side), a
// caller-supplied base64 blob, or a server-local file path (rare). URL/base64
// ride inline as base64; a path is forwarded to the bridge as media_path. A
// local *user* path can't work — this MCP server is remote and can't read the
// user's disk.
func sendMediaHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in sendMediaInput,
) (*mcp.CallToolResult, map[string]any, error) {
	if in.Recipient == "" {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "recipient is required"}, nil
	}
	hasURL := in.URL != nil && strings.TrimSpace(*in.URL) != ""
	hasB64 := in.Base64 != nil && strings.TrimSpace(*in.Base64) != ""
	hasPath := in.MediaPath != nil && strings.TrimSpace(*in.MediaPath) != ""
	if n := boolToInt(hasURL) + boolToInt(hasB64) + boolToInt(hasPath); n != 1 {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "provide exactly one of url, base64 or media_path"}, nil
	}

	var b64, filename string
	if in.Filename != nil {
		filename = strings.TrimSpace(*in.Filename)
	}

	// Server-local path: forward as-is; the bridge reads + validates it. Caption
	// rides along as the message.
	if hasPath {
		absPath, err := filepath.Abs(strings.TrimSpace(*in.MediaPath))
		if err != nil {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("invalid path: %v", err)}, nil
		}
		payload := map[string]any{"recipient": in.Recipient, "media_path": absPath, "no_delay": true}
		if in.Caption != nil && *in.Caption != "" {
			payload["message"] = *in.Caption
		}
		if _, err := callAPI(http.MethodPost, "/send", payload); err != nil {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": err.Error()}, nil
		}
		return &mcp.CallToolResult{}, map[string]any{"success": true, "media_path": absPath}, nil
	}

	if hasURL {
		u := strings.TrimSpace(*in.URL)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "url must be http(s)"}, nil
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": err.Error()}, nil
		}
		client := &http.Client{Timeout: apiTimeout}
		resp, err := client.Do(httpReq)
		if err != nil {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("failed to fetch url: %v", err)}, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("url fetch returned %d", resp.StatusCode)}, nil
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
		if err != nil {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("failed to read url body: %v", err)}, nil
		}
		if len(data) > maxMediaBytes {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "media exceeds size cap"}, nil
		}
		b64 = base64.StdEncoding.EncodeToString(data)
		// Derive a filename (the bridge sniffs media type by extension) from the
		// URL path, then the content-type, falling back to a generic name.
		if filename == "" {
			if base := path.Base(u); base != "" && base != "/" && base != "." && strings.Contains(base, ".") {
				filename = base
			} else if ext := extFromContentType(resp.Header.Get("Content-Type")); ext != "" {
				filename = "media" + ext
			} else {
				filename = "media"
			}
		}
	} else {
		b64 = strings.TrimSpace(*in.Base64)
		// Tolerate a data: URI prefix the caller may have included.
		if i := strings.Index(b64, ","); strings.HasPrefix(b64, "data:") && i != -1 {
			b64 = b64[i+1:]
		}
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "base64 is not valid"}, nil
		}
		if filename == "" {
			filename = "media"
		}
	}

	payload := map[string]any{
		"recipient":      in.Recipient,
		"media_base64":   b64,
		"media_filename": filename,
		"no_delay":       true,
	}
	if in.Caption != nil && *in.Caption != "" {
		payload["message"] = *in.Caption
	}

	if _, err := callAPI(http.MethodPost, "/send", payload); err != nil {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": err.Error()}, nil
	}
	return &mcp.CallToolResult{}, map[string]any{"success": true, "filename": filename}, nil
}

func extFromContentType(ct string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0])) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	default:
		return ""
	}
}

func sendAudioMessageHandler(ctx context.Context,
	req *mcp.CallToolRequest,
	in sendAudioMessageInput) (*mcp.CallToolResult, map[string]any, error) {

	success, msg := SendAudioVoiceMessage(in.Recipient, in.MediaPath)

	resultData := map[string]any{"success": success, "message": msg}

	return &mcp.CallToolResult{IsError: !success}, resultData, nil
}

func getLoginStatusHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	_ getLoginStatusInput,
) (*mcp.CallToolResult, any, error) {
	data, err := callAPI(http.MethodGet, "/auth/status", nil)
	if err != nil {
		return ErrResult(fmt.Sprintf("failed to fetch login status: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil, nil
}

func getPairingQrHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	_ getPairingQrInput,
) (*mcp.CallToolResult, any, error) {
	token, err := GetOrRefreshJwtToken()
	if err != nil {
		return ErrResult(fmt.Sprintf("authentication failed: %v", err)), nil, nil
	}
	httpReq, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/auth/pairing-qr", apiBaseURL), nil)
	if err != nil {
		return ErrResult(fmt.Sprintf("failed to build request: %v", err)), nil, nil
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	client := &http.Client{Timeout: apiTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return ErrResult(fmt.Sprintf("request failed: %v", err)), nil, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.ImageContent{Data: body, MIMEType: "image/png"}},
		}, nil, nil
	case http.StatusGone:
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No pairing QR available. The bridge is either already logged in or has not started the pairing flow yet."}},
		}, nil, nil
	default:
		return ErrResult(fmt.Sprintf("unexpected status %d: %s", resp.StatusCode, string(body))), nil, nil
	}
}

// ── Group management handlers ───────────────────────────────────────────────

func createGroupHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in createGroupInput,
) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Name) == "" {
		return ErrResult("name is required"), nil, nil
	}
	if len(in.Participants) == 0 {
		return ErrResult("at least one participant is required"), nil, nil
	}
	data, err := callAPI(http.MethodPost, "/group/create", map[string]any{
		"name":         in.Name,
		"participants": in.Participants,
	})
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid create-group response"), nil, nil
	}
	return OkResult(result), nil, nil
}

func listGroupsHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	_ listGroupsInput,
) (*mcp.CallToolResult, any, error) {
	data, err := callAPI(http.MethodGet, "/groups", nil)
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result struct {
		Groups []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid list-groups response"), nil, nil
	}
	return OkResult(result.Groups), nil, nil
}

// doUpdateParticipants is the shared body for the four participant actions.
func doUpdateParticipants(in groupParticipantsInput, action string) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}
	if len(in.Participants) == 0 {
		return ErrResult("at least one participant is required"), nil, nil
	}
	data, err := callAPI(http.MethodPost, "/group/update-participants", map[string]any{
		"jid":          in.Jid,
		"participants": in.Participants,
		"action":       action,
	})
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid update-participants response"), nil, nil
	}
	return OkResult(result), nil, nil
}

// maxShareHistoryCount is WhatsApp's own ceiling for a history-sync share. Used
// as the window when share_history is on without an explicit count — the bridge
// then shares however many recent messages there actually are, up to this cap.
const maxShareHistoryCount = 100

func addGroupParticipantsHandler(ctx context.Context, req *mcp.CallToolRequest, in addGroupParticipantsInput) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}
	if len(in.Participants) == 0 {
		return ErrResult("at least one participant is required"), nil, nil
	}
	data, err := callAPI(http.MethodPost, "/group/update-participants", map[string]any{
		"jid":          in.Jid,
		"participants": in.Participants,
		"action":       "add",
	})
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid update-participants response"), nil, nil
	}

	// Optionally share recent history with the members that were actually added.
	// An explicit count wins; otherwise share_history shares the full window and
	// the bridge sends however many recent messages exist (up to the cap).
	shareCount := in.ShareHistoryCount
	if shareCount <= 0 && in.ShareHistory {
		shareCount = maxShareHistoryCount
	}
	if shareCount > maxShareHistoryCount {
		shareCount = maxShareHistoryCount
	}
	if shareCount > 0 {
		if added := addedParticipantJIDs(result); len(added) > 0 {
			if _, err := callAPI(http.MethodPost, "/group/share-history", map[string]any{
				"jid":          in.Jid,
				"participants": added,
				"count":        shareCount,
			}); err != nil {
				// Don't fail the add if the history share couldn't be sent — surface it.
				result["history"] = map[string]any{"ok": false, "error": err.Error()}
			} else {
				result["history"] = map[string]any{"ok": true, "shared_with": len(added)}
			}
		} else {
			result["history"] = map[string]any{"ok": false, "error": "no members were added"}
		}
	}
	return OkResult(result), nil, nil
}

// addedParticipantJIDs pulls the JIDs of members that were added successfully
// (error code 0) from an update-participants response, so we only share history
// with people who actually joined.
func addedParticipantJIDs(result map[string]any) []string {
	rawResults, ok := result["results"].([]any)
	if !ok {
		return nil
	}
	added := make([]string, 0, len(rawResults))
	for _, r := range rawResults {
		entry, ok := r.(map[string]any)
		if !ok {
			continue
		}
		// JSON numbers decode as float64; 0 = success.
		if code, ok := entry["error"].(float64); !ok || code != 0 {
			continue
		}
		if jid, ok := entry["jid"].(string); ok && jid != "" {
			added = append(added, jid)
		}
	}
	return added
}

func removeGroupParticipantsHandler(ctx context.Context, req *mcp.CallToolRequest, in groupParticipantsInput) (*mcp.CallToolResult, any, error) {
	return doUpdateParticipants(in, "remove")
}

func promoteGroupAdminsHandler(ctx context.Context, req *mcp.CallToolRequest, in groupParticipantsInput) (*mcp.CallToolResult, any, error) {
	return doUpdateParticipants(in, "promote")
}

func demoteGroupAdminsHandler(ctx context.Context, req *mcp.CallToolRequest, in groupParticipantsInput) (*mcp.CallToolResult, any, error) {
	return doUpdateParticipants(in, "demote")
}

func setGroupNameHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in setGroupNameInput,
) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" || strings.TrimSpace(in.Name) == "" {
		return ErrResult("jid and name are required"), nil, nil
	}
	if _, err := callAPI(http.MethodPost, "/group/name", map[string]any{"jid": in.Jid, "name": in.Name}); err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	return OkResult(map[string]any{"ok": true}), nil, nil
}

func setGroupTopicHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in setGroupTopicInput,
) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}
	if _, err := callAPI(http.MethodPost, "/group/topic", map[string]any{"jid": in.Jid, "topic": in.Topic}); err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	return OkResult(map[string]any{"ok": true}), nil, nil
}

func getGroupInviteLinkHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in groupInviteLinkInput,
) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}
	data, err := callAPI(http.MethodPost, "/group/invite-link", map[string]any{"jid": in.Jid, "reset": in.Reset})
	if err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return ErrResult("invalid invite-link response"), nil, nil
	}
	return OkResult(result), nil, nil
}

func leaveGroupHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in leaveGroupInput,
) (*mcp.CallToolResult, any, error) {
	if in.Jid == "" {
		return ErrResult("jid is required"), nil, nil
	}
	if _, err := callAPI(http.MethodPost, "/group/leave", map[string]any{"jid": in.Jid}); err != nil {
		return ErrResult(err.Error()), nil, nil
	}
	return OkResult(map[string]any{"ok": true}), nil, nil
}

type uploadMediaInput struct {
	Filename string  `json:"filename" jsonschema:"description:Filename with extension (e.g. video.mp4); drives the WhatsApp media type."`
	Base64   *string `json:"base64,omitempty" jsonschema:"description:Optional raw base64 of the file bytes (no data: prefix). If given, the server uploads it and returns a ready url. Omit to get an upload slot (put_url) to PUT the bytes to yourself — preferred for large files."`
}

// uploadMediaHandler stores a file in the shared temp bucket (via the manager's
// presigning endpoint) and returns a short-lived URL for send_media. The remote
// MCP server can't read the user's disk, so callers either hand over the bytes
// as base64 (mode a) or take an upload slot and PUT the bytes themselves (mode
// b — preferred for large files). Signing stays server-side; this tool only
// makes HTTP requests.
func uploadMediaHandler(
	ctx context.Context,
	req *mcp.CallToolRequest,
	in uploadMediaInput,
) (*mcp.CallToolResult, map[string]any, error) {
	if strings.TrimSpace(in.Filename) == "" {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "filename is required"}, nil
	}
	base := strings.TrimRight(ReadEnv("WEBHOOK_BASE_URL", ""), "/")
	if base == "" {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "media upload not available (WEBHOOK_BASE_URL unset)"}, nil
	}
	secret := ReadEnv("INTERNAL_SECRET", "")
	if secret == "" {
		secret = ReadEnv("BRIDGE_JWT_SECRET", "")
	}
	if secret == "" {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "media upload not available (no internal secret)"}, nil
	}

	// 1. Ask the manager for a presigned upload slot.
	reqBody, _ := json.Marshal(map[string]any{"filename": strings.TrimSpace(in.Filename)})
	slotReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/media/upload-url", bytes.NewReader(reqBody))
	if err != nil {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": err.Error()}, nil
	}
	slotReq.Header.Set("Content-Type", "application/json")
	slotReq.Header.Set("x-internal-secret", secret)
	client := &http.Client{Timeout: apiTimeout}
	slotResp, err := client.Do(slotReq)
	if err != nil {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("upload-url request failed: %v", err)}, nil
	}
	defer slotResp.Body.Close()
	slotBytes, _ := io.ReadAll(slotResp.Body)
	if slotResp.StatusCode >= 300 {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("upload-url returned %d: %s", slotResp.StatusCode, string(slotBytes))}, nil
	}
	var slot struct {
		PutURL string `json:"put_url"`
		GetURL string `json:"get_url"`
	}
	if jerr := json.Unmarshal(slotBytes, &slot); jerr != nil || slot.PutURL == "" || slot.GetURL == "" {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "invalid upload-url response"}, nil
	}

	// 2. No bytes supplied → hand back the slot for the caller to PUT to.
	if in.Base64 == nil || strings.TrimSpace(*in.Base64) == "" {
		return &mcp.CallToolResult{}, map[string]any{
			"success": true,
			"put_url": slot.PutURL,
			"get_url": slot.GetURL,
			"hint":    "PUT the raw file bytes to put_url (no special headers), then call send_media with url=get_url",
		}, nil
	}

	// 3. Bytes supplied → upload them ourselves and return the ready url.
	b64 := strings.TrimSpace(*in.Base64)
	if i := strings.Index(b64, ","); strings.HasPrefix(b64, "data:") && i != -1 {
		b64 = b64[i+1:]
	}
	data, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": "base64 is not valid"}, nil
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, slot.PutURL, bytes.NewReader(data))
	if err != nil {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": err.Error()}, nil
	}
	putResp, err := client.Do(putReq)
	if err != nil {
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("upload PUT failed: %v", err)}, nil
	}
	defer putResp.Body.Close()
	if putResp.StatusCode >= 300 {
		msg, _ := io.ReadAll(putResp.Body)
		return &mcp.CallToolResult{IsError: true}, map[string]any{"success": false, "error": fmt.Sprintf("upload PUT returned %d: %s", putResp.StatusCode, string(msg))}, nil
	}
	return &mcp.CallToolResult{}, map[string]any{"success": true, "url": slot.GetURL}, nil
}
