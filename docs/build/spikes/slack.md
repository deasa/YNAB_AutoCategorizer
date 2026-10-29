# Spike M0-3: Slack Socket Mode

Program: `spikes/slack/` (own `go.mod`, `github.com/slack-go/slack` v0.32.0 + `socketmode`). Run on the owner's Mac on
2026-10-28 against the private channel `#finbot-finances`. Messages posted: 2 (one TEST question, one threaded ack).
All ids, names and emails below are placeholders; the program's log is redacted at write time (`redact_test.go`).

## Findings

| # | Finding | Status |
|---|---------|--------|
| F1 | Socket Mode connects with the `xapp-` token (`connecting → connected → hello` in ~0.9s). Envelopes are acked with `smc.Ack(*evt.Request)` before handling. | VERIFIED |
| F2 | The private channel id is resolved at runtime with `users.conversations?types=private_channel` (needs `groups:read`); `is_private=true`. | VERIFIED |
| F3 | `chat.postMessage` with Block Kit (section + actions block: 3 category buttons + "other…") works in the private channel with `chat:write`. | VERIFIED |
| F4 | A real button click arrives as Socket Mode `interactive` / `block_actions`. Click → receive latency was ~0.1s: `action_ts` vs. local receive time measured -114ms, i.e. under the local/Slack clock skew. | VERIFIED |
| F5 | `chat.update` on `container.message_ts` replaces the buttons with the chosen category. No `response_url` is needed. | VERIFIED |
| F6 | **Threaded replies in a private channel need the bot event `message.groups`.** The first reply was confirmed via `conversations.replies` but produced *no* event; after the owner added `message.groups`, the next reply arrived (`channel_type=group`, latency 153ms). | VERIFIED |
| F7 | The bot's own posts are echoed back as `message` events (`bot_id` set). Handlers must drop messages with `bot_id != ""` (or `user == botUserID`) to avoid self-loops. | VERIFIED |
| F8 | `users.info` gives `name`, `real_name` and display name for both humans; **`profile.email` is empty** with current scopes (needs `users:read.email`). The two humans map cleanly onto an in-memory `users` table keyed by Slack user id. | VERIFIED |
| F9 | Reconnect: closing the websocket's TCP conn out from under the client → `incoming_error` → library auto-reconnects (`connecting`, `connected`, `hello`) in **722ms**. No app code is needed. | VERIFIED |
| F10 | Events emitted *while* disconnected are redelivered by Slack (Events API retries) | UNVERIFIED (not tested; retries carry `retry_attempt`). Design for idempotency by `event_id` / `action_ts`. |
| F11 | Slack's periodic server-initiated `disconnect` (refresh) is handled by the same reconnect path. | UNVERIFIED (not observed in two short runs of about 12 and 6 minutes) |
| F12 | Dropping `groups:write`, `channels:read` and `channels:history` breaks nothing in a private-channel-only design. | UNVERIFIED (inferred: no call in the spike needs them) |
| F13 | DMs need `im:history` + bot event `message.im` (receive), and `im:write` to open a DM (`conversations.open`). | UNVERIFIED (from Slack docs, not exercised) |

## Payload shapes (redacted)

**`block_actions`** (Socket Mode `interactive` envelope payload; `blocks`/`state` trimmed):
```json
{"type":"block_actions",
 "user":{"id":"<U_HUMAN>","username":"<NAME>","name":"<NAME>","team_id":"<T_ID>"},
 "api_app_id":"<APP_ID>","token":"<REDACTED>","trigger_id":"<REDACTED>",
 "container":{"type":"message","message_ts":"1793245768.650209","channel_id":"<C_ID>","is_ephemeral":false},
 "team":{"id":"<T_ID>","domain":"<WORKSPACE>"},"enterprise":null,"is_enterprise_install":false,
 "channel":{"id":"<C_ID>","name":"privategroup"},
 "message":{"user":"<U_BOT>","type":"message","ts":"1793245768.650209","bot_id":"<B_ID>","app_id":"<APP_ID>",
            "text":"[SPIKE TEST] categorize TEST: Costco $12.34?","team":"<T_ID>"},
 "response_url":"<URL>",
 "actions":[{"type":"button","block_id":"spike_q","action_id":"cat_pick_1",
             "text":{"type":"plain_text","text":"TEST Discretionary","emoji":false},
             "value":"txn:test-1|cat:discretionary","action_ts":"1793246554.741274"}]}
```
Notes: a private channel's `channel.name` is reported as `"privategroup"`, not its real name. The button `value` (≤2000
chars) is where finbot should carry the transaction id and category id.

**Threaded reply** (Socket Mode `events_api` envelope payload):
```json
{"token":"<REDACTED>","team_id":"<T_ID>","context_team_id":"<T_ID>","context_enterprise_id":null,"api_app_id":"<APP_ID>",
 "event":{"type":"message","user":"<U_HUMAN>","ts":"1793246845.034049","client_msg_id":"<REDACTED>",
          "text":"<free text>","team":"<T_ID>","thread_ts":"1793245768.650209","parent_user_id":"<U_BOT>",
          "channel":"<C_ID>","event_ts":"1793246845.034049","channel_type":"group"},
 "type":"event_callback","event_id":"<EV_ID>","event_time":1793246845,
 "authorizations":[{"enterprise_id":null,"team_id":"<T_ID>","user_id":"<U_BOT>","is_bot":true,"is_enterprise_install":false}],
 "is_ext_shared_channel":false,"event_context":"<REDACTED>"}
```
A reply is matched to its question by `event.thread_ts == question ts` (with `parent_user_id == bot`). Messages that
aren't threaded have no `thread_ts`.

## Required scopes and events

| Need | Scope / event | Used for |
|------|---------------|----------|
| Socket Mode | app-level token with `connections:write` | `apps.connections.open` |
| Post / update questions | `chat:write` | `chat.postMessage`, `chat.update` |
| Find the private channel, list members | `groups:read` | `users.conversations`, `conversations.members` |
| Read thread history (recovery / dedupe) | `groups:history` | `conversations.replies`; also required for `message.groups` |
| Identity | `users:read` | `users.info` |
| Receive private-channel messages | bot event **`message.groups`** (now configured) | threaded replies, free-text intents |
| Button clicks | Interactivity on (no Request URL needed under Socket Mode) | `block_actions` |

- **Drop:** `groups:write` (only for creating or managing private channels; finbot never does that). If finbot stays private-channel-only, also drop
  `channels:read`, `channels:history` and the `message.channels` event (F12, UNVERIFIED but no code path uses them).
- **Optional:** `users:read.email`, only if identity should key on email; Slack user id is sufficient (F8).
- **For DMs later (R11 / §5):** `im:history`, `im:write`, event `message.im` (F13, UNVERIFIED).

## Recommendation

Go with Socket Mode + `slack-go/slack` as planned in §5. Every building block works end to end, latency is sub-second,
and reconnect needs no app code. Implementation rules from the spike:
1. Ack every envelope first, then handle it asynchronously.
2. Ignore the bot's own messages (`bot_id` set) and any `subtype` other than the empty one / `thread_broadcast`.
3. Make click and reply handling idempotent (key on `event_id` for events, `container.message_ts` + `action_ts` for
   clicks), because redelivery after reconnects is plausible (F10).
4. Persist `(channel, message_ts) → question` so that clicks and thread replies can be routed after a restart, and
   `chat.update` the question once it's answered.
5. Map users by Slack user id; don't rely on email.

## Impact on architecture (§5)

- §5 holds as written: Socket Mode, `slack-go/slack`, Block Kit buttons for deterministic answers, threaded or free-text
  replies routed onward (to §5a). No public endpoint is needed.
- **Add to the data model (§2):** a `slack_questions` table (`channel_id`, `message_ts`, `kind`, subject id e.g. txn id,
  status, answered_by, answered_at). Thread replies and clicks look up the question by `message_ts`.
- **`users` table:** key on `slack_user_id`; `real_name` comes from `users.info`. Email is not available without an
  extra scope and isn't needed.
- **Setup checklist / ops (§6):** the app needs bot event `message.groups` (it wasn't configured initially; without it
  replies are silently dropped). Add a startup self-check: on boot, verify that the configured channel resolves via `users.conversations`.
- **Scopes:** remove `groups:write`; decide whether public-channel scopes stay (only if a public channel is ever used).
- **Dedupe:** the categorize/apply path triggered from Slack must be idempotent per question (see Recommendation 3).
