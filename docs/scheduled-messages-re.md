# WhatsApp native scheduled messages: reverse engineering notes

Status: research done; experimental text-only implementation in the bridge (`whatsapp-bridge/scheduled.go`), live-tested once (§11). No live traffic was captured.

## TL;DR

A native scheduled message is **sent to everyone immediately**, sealed. The reveal key goes **only to
WhatsApp's server**, in the send stanza's `<meta>`. At the scheduled time the server pushes the key to
the recipients in a `mex` notification, and their clients decrypt and show the message. So the
sender (our bridge) can be offline at delivery time: the server holds the key and the recipients
already hold the ciphertext.

| Step | What happens | Confidence |
|---|---|---|
| Crypto | Random 32-byte key, random 12-byte IV, AES-256-GCM over the serialized inner `waE2E.Message`, no AAD, no HKDF. *Not* `SecretEncryptedMessage`/`MESSAGE_SCHEDULE` and not message-secret-derived. | High |
| Wrapper | `Message{conditionalRevealMessage{encPayload, encIV, conditionalRevealMessageType=SCHEDULED_MESSAGE, revealKeyID=<UUID>}, messageContextInfo{messageSecret}}` | High |
| Stanza | Normal E2E `<message>` fanout. Extra child `<meta type="scheduled_message" st="<unix s>"><key rkid="<UUID>">32 raw key bytes</key></meta>`. `type` = the *inner* message's type (`text` or `media`); `<enc mediatype>` = the inner media type. | High |
| Delivery | Ciphertext goes to recipients now; they store it as "reveal pending" and show nothing. The sender's other devices get the same stanza **including** the meta with the key, so the phone lists it as scheduled. | High |
| Reveal | At `st`, the server sends recipients a `mex` notification `NotificationScheduledMessageReveal` → `xwa2_notify_scheduled_message_reveal {chat_jid, rk (url-safe base64 key), rkid, st, sender_user_jid}`. The sender gets `NotificationScheduledMessagePost` → `xwa2_notify_scheduled_message_post {rkid, status: SUCCESS | error, chat_jid, error_code}`. | High (shape), medium (exact XML wrapper) |
| Cancel | `ProtocolMessage{key: <original key>, type: MESSAGE_UNSCHEDULE (32)}` sent with `edit="7"` (sender revoke) and `<meta type="scheduled_message"><key rkid="<UUID>"/></meta>` (no key bytes, no `st`). Receivers treat it as a sender revoke of the hidden message. | High (WA Web), see note on `meta` |
| Edit | No native edit. WA Web's "retry"/"change" = unschedule + schedule again; "send now" = send the text normally and drop the local key. | High |
| Limits | 30 pending per chat (server NACK **419** beyond that); window from AB props `scheduled_messages_window_duration_{min,max}_seconds`; WA Web's own flag defaults are **600 s (10 min, measured from the start of the current minute) to 1,209,600 s (14 days)**; reveal keys kept 30 days; text plus at most 1 image/video (media behind AB prop `scheduled_messages_photo_video_sender_enabled`). DMs and groups; not newsletters, broadcasts, bots, community announcement groups, Cloud-API/hosted contacts. | High |

## Sources

1. **WhatsApp Web JS modules**, daily dump in `vinikjkkj/wa-diff`, commit
   `6a0339e8ec7c7e3a155de39e49e497f90a443829` (2026-11-26 01:20 UTC). Files are prettified single modules
   (`files/<Module>.js`), so references below are `Module.js:line` in that commit. Local copies (untrusted,
   not executed): `C:\Users\User\AppData\Local\Temp\claude\4770acc3d7bcc157\3475f8b0-8725-41c5-8a80-899faf030c4f\scratchpad\wadiff\`.
2. **Chomosuke9/WazzapClients** `internal/wa/schedule.go` (Go, on `polymorfa/hypermeow`, a whatsmeow fork,
   pushed 2026-11-25): a working send/receive/unschedule implementation. Confirms the flow and gives
   the time window and the `edit="7"` detail.
3. **Baileys forks** (`rexxzyid/elaina-baileys` `src/Utils/scheduled-message.js`, also `LyncxTeams/Baileys`,
   `vanzxysenpai`): crypto + meta-node helpers, same constants, window `chat: 600..1209600`,
   `newsletter: 600..2592000`.
4. **whatsmeow protos** (f16f9ca and the bridge's 9399289): `ConditionalRevealMessage` (field 120 of
   `Message`), `ProtocolMessage.Type.MESSAGE_UNSCHEDULE = 32`, `waWeb.ScheduledMessageMetadata`
   (field 81 of `WebMessageInfo`, history sync), `SCHEDULED_MESSAGE_CREATED = 225` stub.
   Upstream whatsmeow (tulir, HEAD 10b99a9 2026-11-25) and upstream Baileys have **no** scheduled-message
   code or issues.
5. **jlucaso1/whatsapp-rust** (HEAD 6e89a56, 2026-11-26; shallow clone, not built). No send/receive/unschedule
   code, no PRs or issues. What it does have:
   - `wacore/src/iq/abprops.rs:11058-11093`, auto-generated from WA Web 2.3000.1054751086's AB-prop registry:
     `scheduled_messages_sender_enabled` (code 23845), `..._receiver_enabled` (24610),
     `..._photo_video_sender_enabled` (32553), `..._smart_defaults` (36583), all default **false**;
     `..._window_duration_min_seconds` (26348) default **600**, `..._window_duration_max_seconds` (26347)
     default **1209600**. This confirms the window from WhatsApp's own registry, and shows the feature is
     off unless the server turns it on for the account.
   - `wacore/src/send/classify.rs:481` hides decrypt failures of `conditionalRevealMessage` (same as WA Web).
   - `src/features/message_edit.rs:288-316` deliberately excludes `SecretEncType::MESSAGE_SCHEDULE` because
     WA Web assigns it no use-case secret, which matches our finding that scheduling doesn't use that path.

## 1. Building and encrypting

`WAWebScheduledMsgSenderJob.prepareScheduledMessage` (`WAWebScheduledMsgSenderJob.js:19-119`):

1. `revealKey = getRandomValues(32 bytes)` (`WAWebScheduledMsgCrypto.js:11-16`, `SCHEDULED_MSG_REVEAL_KEY_BYTES = 32`,
   `WAWebScheduledMsgConstants.js:14`).
2. `revealKeyId = crypto.randomUUID()` (`WAWebScheduledMsgCrypto.js:17-19`). Lower-case UUID string.
   (WazzapClients uses 32 upper-case hex chars "as phones make them"; the server treats it as an opaque string.)
3. Inner message = the normal message proto with `messageContextInfo.messageSecret` set (reuses the existing
   secret or makes 32 random bytes, `WAWebScheduledMsgSenderJob.js:38-47`).
4. `encPayload||tag = AES-256-GCM(key=revealKey, iv=12 random bytes, plaintext=encode(inner))`, no AAD
   (`WAWebScheduledMsgCrypto.js:30-55`, IV size `WAWebScheduledMsgConstants.js:15`). WebCrypto appends the
   16-byte tag to the ciphertext, same as Go's `cipher.AEAD.Seal`.
5. Outer message (`WAWebScheduledMsgSenderJob.js:58-69`):
   ```
   conditionalRevealMessage { conditionalRevealMessageType: SCHEDULED_MESSAGE, encIv, encPayload, revealKeyId }
   messageContextInfo       { messageSecret: <same secret as inside> }
   ```
6. Stored locally first; refuses when the chat already has 30 pending (`WAWebScheduledMsgStore.js`
   `isChatAtScheduleLimit`, `SCHEDULED_MSG_MAX_PER_CHAT = 30`).
7. Reporting token is computed over the **inner** bytes (`WAWebScheduledMsgSenderJob.js:49-57`).

The `SecretEncryptedMessage.MESSAGE_SCHEDULE = 3` enum is **not used** by the scheduling path in this
bundle (no module references it). The guess in the task brief (message-secret HKDF like event RSVPs) is wrong.

## 2. The send stanza

`maybeWrapScheduledMessageForSend` (`WAWebScheduledMsgSenderJob.js:120-165`) returns
`scheduledMsgMetadata = {kind:"schedule", scheduledTimestampS, revealKeyId, revealKey, originalStanzaType,
originalMediaType, originalMessageAssociationType}`. Then:

- `<message type=...>` uses `originalStanzaType` (`WAWebSendMsgCreateFanoutStanza.js:949`), i.e. what the
  inner message would have had. Receivers reject anything other than `text` or `media`
  (`WAWebConditionalRevealPreProcessor.js:58-67`, error `INVALID_SCHEDULED_MESSAGE_STANZA`).
- `<enc mediatype=...>` uses `originalMediaType` (`WAWebSendMsgCreateFanoutStanza.js:99-102`, `:169-171`).
- `<meta>` (`WAWebSendMsgMetaNode.js:120-189`, contributor `WAWebScheduledMsgStanzaContributor.js`,
  smax schema `WASmaxOutMessagePublishMetaScheduledMessageMixin.js:9-20`):
  ```xml
  <meta type="scheduled_message" st="1764150000">
    <key rkid="2b1f…-uuid">[32 raw bytes]</key>
  </meta>
  ```
  Other meta attributes (e.g. `view_once`) share the same `<meta>` element.
- Everything else (participants, `<enc>` per device, `device-identity`, reporting token) is a normal send.

Group sends go through the normal sender-key path with the same meta (`WAWebUnscheduleMsgAction`/
`WAWebScheduledMsgUnscheduleDirectSend.js` pick group vs user send; the reveal handler has a group branch).

## 3. Who gets what, and when

- **Recipients** get the ciphertext now. `WAWebScheduledMsgReceivePreProcess.preProcessIncomingScheduledMsg`
  → `WAWebConditionalRevealPreProcessor` stores a `PENDING` record with an **empty key**
  (`WAWebConditionalRevealPreProcessor.js:260-319` store, `:320-391` entry) and skips normal processing: nothing shows, no
  notification. If the key already arrived (orphan key store), it decrypts at once (`:186-259`).
- **Sender's own devices** get the stanza with the meta *and the key*: the incoming schema
  `WASmaxInMessageDeliverMetaScheduledMessageMixin.js` requires `type="scheduled_message"`, `st` in
  [1577865600, 4102473600] and a 32-byte `<key rkid>`; `WASmaxInMessageDeliverDeliverLinkedDeviceScheduleMessageMixin.js`
  is the linked-device variant. The client stores it in its scheduled list and inserts a
  "scheduled message created" system bubble (`WAWebConditionalRevealPreProcessor.js:86-185`). **This is
  how the phone learns a bridge-scheduled message exists and can show/cancel it.**
- History sync carries the key for your own scheduled messages in `WebMessageInfo.scheduledMessageMetadata`
  (`waWeb` field 81; `WAWebHistorySyncScheduledMsgInnerProtoBuilder.js`).

## 4. Reveal

`WAWebScheduledMsgMexNotificationHandlers.js` maps two mex operations:

- `NotificationScheduledMessageReveal` → `WAWebScheduledMsgRevealNotificationHandler.mexHandleScheduledMsgReveal`:
  reads `xwa2_notify_scheduled_message_reveal {chat_jid, rk, rkid, st}` (`:79-95`), `rk` is url-safe base64
  (`:252-258`), finds the pending record by `rkid`, checks the chat matches, decrypts, builds a normal
  incoming message with id = original stanza id, `t = st`, sender = original sender (`:354-562` decrypt + insert, `:563-656` message model), inserts it,
  deletes the key. Unknown `rkid` → stored as an orphan key (`:657-675`) for a ciphertext that arrives later.
- `NotificationScheduledMessagePost` → `WAWebScheduledMsgPostNotificationHandler.mexHandleScheduledMsgPost`
  (sender side): `xwa2_notify_scheduled_message_post {rkid, status}` (`:39-155`). `SUCCESS` turns the local
  pending bubble into a sent message; anything else marks it `FAILED` (`:156-438` success, `:439-455` failure).

Wire shape (from WazzapClients' parser, medium confidence): `<notification type="mex" from="<sender or chat>">
<update op_name="NotificationScheduledMessageReveal">{"data":{"xwa2_notify_scheduled_message_reveal":{…}}}</update></notification>`.
WazzapClients also gets `sender_user_jid`.

The sender's device does **nothing** at reveal time. The server is the clock.

## 5. Cancel (unschedule)

`WAWebUnscheduleMsgAction.unscheduleMsgAction` (`WAWebUnscheduleMsgAction.js`) →
`WAWebScheduledMsgUnscheduleDirectSend.sendUnscheduleStanza`:

- Body: `ProtocolMessage{key: {remoteJid: chat, fromMe: true, id: <original id>, participant: me in groups},
  type: MESSAGE_UNSCHEDULE}` (`WAWebGenerateProtocolUnscheduleMessageProto.js`).
- `edit="7"` (sender revoke): `WAWebSendMsgCommonApi.js:140-145` maps `MESSAGE_UNSCHEDULE` → `EDIT_ATTR.SENDER_REVOKE`;
  the log line says `send edit="7"` (`WAWebScheduledMsgUnscheduleDirectSend.js:22`).
- Meta: `scheduledMsgMetadata = {kind:"unschedule", revealKeyId}` (`:31`) → `<meta type="scheduled_message"><key rkid="…"/></meta>`
  (no `st`, empty key; `WASmaxOutMessagePublishMetaUnscheduleMessageMixin.js`). WazzapClients sends it
  **without** meta and says it works, so the server presumably matches by message id too. We follow WA Web.
- Receivers (`WAWebScheduledMsgUnscheduleHandler.js`) delete the pending record and run a `sender_revoke`.
  Receivers hide `MESSAGE_UNSCHEDULE` decrypt failures (`WAWebE2EProtoUtils.js:221-245`).

Edit / reschedule / send-now: `WAWebRetryScheduledMsgAction.js:66` re-sends a fresh scheduled text with the
same time; `WAWebSendScheduledMsgNowAction.js:66` sends the plain text and drops the key. There is no
in-place edit.

## 6. Limits and gating

- `SCHEDULED_MSG_MAX_PER_CHAT = 30`, NACK `419` when the server's own limit is hit, keys retained 30 days,
  `MAX_MEDIA_MSGS_TO_SCHEDULE = 1`, schedulable media = image, video (`WAWebScheduledMsgConstants.js`).
- Time check (`WAWebIsScheduledTimeValid.js:7-15`): `st - floor(now/60)*60 >= min && st - now <= max`, with
  min/max from AB props, defaults 600 s and 1,209,600 s (WA Web registry via whatsapp-rust; the two other clients agree). WA Web schedules
  to the minute.
- Sender gate: AB prop `scheduled_messages_sender_enabled`; receiver gate `scheduled_messages_receiver_enabled`
  (`WAWebScheduledMessagesGatingUtils.js`). **Receivers without the receiver gate drop or mis-render the
  message** (they would see an unsupported/hidden message). Both gates default to **false**, so it works only where the server enables them; rollout for Joy's/Lee's accounts unknown (a phone showing "Schedule message" in its send menu means the sender gate is on).
- Chats: not bots, newsletters, community announcement groups (CAG), Cloud-API-hosted groups or contacts,
  or "me/hosted" chats (`WAWebIsScheduledMessagesAvailableForChat.js`). Groups allowed; UI warns for
  admin-only, suspended or left groups (`WAWebScheduledMsgDeliverability.js`). Not business-only.

## 7. What whatsmeow lacks (for Phase 2)

| Need | whatsmeow (bridge pins 9399289) | Plan |
|---|---|---|
| Protos | `ConditionalRevealMessage`, `MESSAGE_UNSCHEDULE` present | use as is |
| `<meta>` child on send | `SendRequestExtra.AdditionalNodes` appends nodes to `<message>` | use it (no `Meta` set, so no second `<meta>`) |
| `type="text"` for a text inner | `getTypeFromMessage` defaults to `text` for ConditionalReveal | OK for text |
| `type="media"` + `<enc mediatype>` for media | computed from the outer message → `text`, no mediatype | **text only** in the experiment; media needs a whatsmeow patch |
| `edit="7"` on `MESSAGE_UNSCHEDULE` | `getEditAttribute` returns empty | send without; if the server ignores the cancel, add a one-line whatsmeow patch (replace directive) — WazzapClients had to patch this |
| Reveal/post `mex` notifications | not parsed | log them (optional) |

## 8. Risks for production

- Undocumented, AB-gated protocol; WhatsApp changed the unschedule meta at least once (WazzapClients vs current Web).
- Recipients on old clients or without the receiver gate may never see the message, and the sender is not told
  (only the `post` notification would say so).
- Cancel without `edit="7"` is unverified.
- Wrong stanza type for media would be rejected by receivers (`INVALID_SCHEDULED_MESSAGE_STANZA`).
- The bridge cannot list or verify server state; it only knows what it sent.

## 9. Observed on a real account

2026-11-26: WhatsApp Desktop on Lee's account offers "Schedule message" and accepts a time only 10 or more
minutes ahead (scheduled 15:21 → 15:31, chat shows "You scheduled a message. Click to view" and lists it under
"Scheduled messages"). So the sender gate is on for that account and the 10-minute minimum holds.
At 15:31 the message appeared in the chat as a normal sent text stamped 15:31 with double ticks (delivered),
and the Scheduled messages panel read "All scheduled messages have been sent or deleted". The "You scheduled a
message" system bubbles stay in the chat. This matches the bridge's choice to put a posted message into history
at its scheduled time, not its send time.

## 10. Implementation in this fork (experimental)

`whatsapp-bridge/scheduled.go`:

- `POST /api/send` takes `schedule_at` (RFC3339, `YYYY-MM-DD HH:MM` in the bridge's timezone, or unix seconds).
  Text only (rejects media, view-once, `send_as`); people and groups only; time rounded down to the minute and
  checked against the 10 min / 14 day window; refuses a 31st pending message per chat (the server would NACK 419).
  The built message (reply, mentions, link preview included) is sealed (`sealScheduledMessage`) and sent with
  `SendRequestExtra.AdditionalNodes = [<meta type="scheduled_message" st><key rkid>key</key></meta>]`. No typing delay.
- The send is recorded in `scheduled_messages` (id, chat, text, reveal key id and key, time, status
  `scheduled` → `posted` | `cancelled`), not in `messages`. A sweeper (every 30 s) copies each one whose time
  came into `messages`, timestamped at its scheduled time, and marks it `posted`. With the patched whatsmeow
  (section 12) a `NotificationScheduledMessagePost` with a failure status marks it `failed` (and the `messages`
  row failed); on stock whatsmeow "posted" means only that its time passed.
- `GET /api/scheduled?chat_jid=&status=` lists rows (default `scheduled`, `all` for every status).
- `POST /api/unschedule {recipient, message_id}` sends `ProtocolMessage MESSAGE_UNSCHEDULE` naming it, with
  `<meta type="scheduled_message"><key rkid/></meta>`, then marks the row `cancelled`. **Sent without `edit="7"`**
  (stock whatsmeow can't set it). If the live test shows the cancel is ignored, the fix is one case in whatsmeow's
  `getEditAttribute` (`ProtocolMessage_MESSAGE_UNSCHEDULE` → `EditAttributeSenderRevoke`).
- MCP: `send_message.schedule_at`, `list_scheduled_messages`, `cancel_scheduled_message`.
- Incoming and phone-scheduled messages: see section 12.

Live test checklist (Phase 3): phone lists the message as scheduled; it arrives on time with the bridge stopped;
recipient sees normal text (not "unsupported"); cancel removes it from the phone's list and the recipient never
gets it; a reply/mention survives the seal.

## 11. Live test 2026-11-26 (bridge on Joy 60165343717 → Lee 60165420926)

- 16:18:48 `POST /api/send` with `schedule_at` 16:31 → server accepted (no NACK), id `3EB04AFD2045A657597938`,
  rkid `d9192f71-…`. Bridge stopped at 16:19:19 and stayed off.
- Lee's phone showed nothing before the time, then received **"Scheduled test 1 (bridge)" as a normal text**
  after 16:31. Offline delivery by the server: **confirmed**. Joy's account (no Desktop check) can schedule.
- 16:19:01 test 2 scheduled for 16:33, cancelled 16:19:10 (`MESSAGE_UNSCHEDULE`, `edit="7"`, meta with rkid) → server
  accepted the cancel. Whether it stayed undelivered: see below.
- Found and fixed during the test: times bound with an offset other than local were stored wrong in Postgres
  `TIMESTAMP`; `/unschedule` didn't take a bare phone number.
- Neither wa-lee nor the local bridges log receipts or `mex` notifications at the default level, so the bridge
  side can't confirm delivery; only the recipient's phone could.

## 12. Incoming and phone-scheduled messages (reveal)

Needs the patched whatsmeow (`patches/whatsmeow.patch` in the consumers, build tag `grouphistory`), which adds:

- `events.MexUpdate{Mex, From, Data}`: every `mex` notification whatsmeow has no typed event for, raw JSON
  included (stock whatsmeow logs and drops them).
- `types.MsgMetaInfo.ScheduledRevealKeyID / ScheduledRevealKey / ScheduledTime`, parsed from
  `<meta type="scheduled_message" st=…><key rkid=…>key</key></meta>` on incoming stanzas.

`whatsapp-bridge/scheduled_reveal.go` (+ `scheduled_reveal_patched.go`, the tagged hooks):

- Every sealed `conditionalRevealMessage` that arrives is kept in `scheduled_incoming` (reveal key id, message id,
  chat, sender, push name, sealed payload, key when known, time, status `pending` → `revealed` | `cancelled`).
  Nothing shows in `messages` until it opens.
- **Someone else's**: the key arrives at the scheduled time as mex `NotificationScheduledMessageReveal`
  (`xwa2_notify_scheduled_message_reveal {chat_jid, rk (url-safe base64), rkid, st, sender_user_jid}`). Key and
  ciphertext can come in either order (offline replay); whichever lands second triggers the reveal. The message
  is decrypted, unwrapped and fed through the normal message handler with its original id, sender and the
  scheduled time, so it is stored, webhooked and shown like any other message. A reveal is claimed once.
- **The owner's, scheduled on the phone** (or another linked device): the stanza carries the key and time. The
  bridge lists it in `scheduled_messages` with `source = 'phone'` and its text (so `list_scheduled_messages` and
  the manager's drawer show it and it can be cancelled from the bridge), stores the usual "scheduled for …"
  notice, and its own 30 s sweeper does **not** post it. It is revealed by `NotificationScheduledMessagePost`
  with `SUCCESS`, or, if that never comes, 2 minutes after its time.
- `MESSAGE_UNSCHEDULE` from the sender (or `/api/unschedule` from us) marks the pending row `cancelled`; it is
  never revealed.
- `NotificationScheduledMessagePost` with a failure status marks one of our own scheduled messages `failed`.
- Reveal runs are recovered from panics; the 30 s sweeper handles the owner's phone-scheduled fallback above.

On stock whatsmeow (no tag) none of the mex/meta hooks exist: incoming sealed messages are still kept, but
never open, exactly as before.
