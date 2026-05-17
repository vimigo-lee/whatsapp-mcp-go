# WhatsApp MCP Channel — Complete Guide

## What Is WhatsApp Channel

A channel is an MCP server that runs on the same machine as Claude Code, spawned as a subprocess communicating over stdio. It is the bridge between external systems (like WhatsApp) and the Claude Code session.

---

## The Problem It Solves

**Without channel** — you have to manually ask Claude every time:
```
User sends WhatsApp message → Claude has no idea
You must type: "hey check my WhatsApp"
```

**With channel** — Claude gets notified automatically:
```
User sends WhatsApp message → Claude reacts immediately
```

---

## Full Architecture

```
Your Mac/PC
┌─────────────────────────────────────────────┐
│                                             │
│  whatsapp-bridge ──────────────────────┐   │
│  (WhatsMeow, always running)           │   │
│  holds WhatsApp WebSocket              │   │
│                                        │   │
│                                        ▼   │
│                          MCP Channel Server │
│                          (port 8788)        │
│                               │            │
│                               │ stdio       │
│                               ▼            │
│                          Claude Code        │
│                          session            │
│                               │            │
│                               ▼            │
│                          Claude reacts      │
│                          sends reply        │
└─────────────────────────────────────────────┘
```

---

## Step by Step Flow

```
1. User sends "approval" on WhatsApp
        │
        ▼
2. whatsapp-bridge detects message
   (WebSocket always open)
        │
        ▼
3. Bridge POSTs to MCP Channel Server
   POST http://localhost:8788
   { sender: "60123456789", text: "approval" }
        │
        ▼
4. Channel Server pushes notification
   into Claude Code session via stdio
        │
        ▼
5. Claude Code wakes up, sees:
   <channel source="whatsapp" sender="60123456789">
   approval
   </channel>
        │
        ▼
6. Claude decides what to do:
   - reply via WhatsApp MCP tool
   - trigger approval workflow
   - ask you for input
```

---

## Three Components — All Must Run

| Component | What It Does | Must Stay On |
|---|---|---|
| **whatsapp-bridge** | Holds WhatsApp WebSocket connection | ✅ Always |
| **MCP Channel Server** | Listens on :8788, pushes to Claude | ✅ Always |
| **Claude Code session** | Reacts to notifications | ✅ While you work |

---

## Key Limitation

```
Claude Code session closed?
        │
        ▼
Channel notifications have nowhere to go
        │
        ▼
Messages are lost ❌
```

> This is NOT auto-reply. Claude only reacts when YOU have Claude Code open.

---

## When Is This Useful?

| Scenario | Works? |
|---|---|
| You are working in Claude Code and WhatsApp message arrives | ✅ |
| Approval workflow — someone sends "approval" while you're busy | ✅ |
| Claude notifies you mid-task without you asking | ✅ |
| You close your laptop | ❌ |
| Full 24/7 auto-reply | ❌ |

---

## MCP Channel vs Other Approaches

| | Manual MCP | MCP Channel | Claude API on VPS |
|---|---|---|---|
| **How triggered** | You ask Claude | WhatsApp message | WhatsApp message |
| **24/7** | ❌ | ❌ | ✅ |
| **Needs Claude Code open** | ✅ | ✅ | ❌ |
| **Auto-reply** | ❌ | Partial | ✅ |
| **Best for** | Ad-hoc queries | Interrupts while working | Full automation |

---

## Channel Server Code (TypeScript/Bun)

```typescript
// whatsapp-channel.ts — runs on YOUR machine
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";

const PORT = 8788;

const mcp = new McpServer(
  { name: "whatsapp", version: "1.0.0" },
  {
    capabilities: {
      experimental: { "claude/channel": {} }  // makes it a channel
    }
  }
);

// HTTP listener — whatsapp-bridge posts here
const server = Bun.serve({
  port: PORT,
  async fetch(req) {
    if (req.method !== "POST") return new Response("POST only", { status: 405 });

    const body = await req.json();

    // Push WhatsApp message into Claude Code session
    mcp.notification({
      method: "notifications/claude/channel",
      params: {
        content: `WhatsApp message from ${body.sender}: ${body.text}`,
        meta: {
          sender: body.sender,
          chat_id: body.chatId,
        }
      }
    });

    return new Response("ok");
  }
});

// Connect to Claude Code via stdio
const transport = new StdioServerTransport();
await mcp.connect(transport);
```

---

## Register Channel in Claude Code

**.mcp.json**
```json
{
  "mcpServers": {
    "whatsapp-channel": {
      "command": "bun",
      "args": ["whatsapp-channel.ts"]
    }
  }
}
```

**.claude/channels.json**
```json
{
  "whatsapp": {
    "server": "whatsapp-channel"
  }
}
```

---

## Bridge Posts to Channel (Go)

```go
// In whatsapp-bridge — when message arrives, notify channel
func handleIncomingMessage(evt *events.Message) {
    body := evt.Message.GetConversation()
    sender := evt.Info.Sender.String()

    http.Post("http://localhost:8788", "application/json",
        strings.NewReader(fmt.Sprintf(
            `{"sender":"%s","text":"%s","chatId":"%s"}`,
            sender, body, evt.Info.Chat.String(),
        )),
    )
}
```

---

## Keeping the Bridge Always Running

### Windows (NSSM)
```bash
nssm install WhatsAppBridge "C:\path\to\whatsapp-bridge.exe"
nssm set WhatsAppBridge AppDirectory "C:\path\to\bridge-folder"
nssm set WhatsAppBridge Start SERVICE_AUTO_START
nssm start WhatsAppBridge
```

### macOS (LaunchAgent)
```xml
<!-- ~/Library/LaunchAgents/com.whatsapp.bridge.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.whatsapp.bridge</string>
    <key>ProgramArguments</key>
    <array>
        <string>/path/to/whatsapp-bridge</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/path/to/bridge-folder</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/whatsapp-bridge.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/whatsapp-bridge-error.log</string>
</dict>
</plist>
```

```bash
# Load once — runs on every boot after
launchctl load ~/Library/LaunchAgents/com.whatsapp.bridge.plist
```

---

## NAT Problem (Local Device)

If bridge is on local machine, VPS cannot reach it directly due to home router NAT.

**Solutions:**

```bash
# Option 1: Cloudflare Tunnel (free, no time limit)
cloudflared tunnel --url http://localhost:8788

# Option 2: ngrok
ngrok http 8788

# Option 3: Tailscale VPN (cleanest)
# Install on both machines → they get private IPs
# VPS posts to http://100.x.x.x:8788 directly

# Option 4: Reverse SSH tunnel
ssh -R 8788:localhost:8788 user@your-vps
```

---

## Bottom Line

> **MCP Channel = WhatsApp can tap Claude on the shoulder while you're working.**

- ✅ Great for approval workflows
- ✅ Great for interrupting Claude mid-task
- ❌ Not 24/7 auto-reply
- ❌ Stops working when Claude Code is closed

For true 24/7 auto-reply → use **Claude API directly on VPS**, no MCP needed.
