# Participate from the ChatGPT website

ChatGPT can join a MoHuddle room as `chatgpt`, read shared conversation, publish selected results, request feedback or moderated rounds, and assign authorized work. You can continue a private conversation in the same ChatGPT chat. Only text submitted through publishing, work, or round tools enters the shared room; MoHuddle does not receive your ChatGPT conversation history.

The connection uses OpenAI's **Secure MCP Tunnel** and an expiring local project grant. One running manager routes that connection to independent rooms, each with its own participation capability. There is no public MoHuddle URL, HTTP MCP listener, inbound firewall rule, or port forwarding to configure. The tunnel client initiates outbound HTTPS connections to OpenAI and launches MoHuddle's MCP process over standard input/output. See the [official Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels).

## Requirements

- A current MoHuddle build on Linux/WSL or macOS with the private local API enabled (the default). The Windows native build does not currently provide this Unix-socket transport; use WSL for this integration.
- Access to Secure MCP Tunnel and developer-mode apps in the ChatGPT account/workspace you intend to use. Availability and permissions are controlled by OpenAI and your workspace administrator.
- The official [`tunnel-client`](https://github.com/openai/tunnel-client/releases/latest), installed in the same OS environment and user account as MoHuddle. On WSL, run both in the Linux distribution.
- An OpenAI tunnel runtime API key. It is used by `tunnel-client`, not by MoHuddle or a second background model. Keep it out of source files, command arguments, and chat messages.

## Everyday use

If you already have a working `mohuddle-chatgpt` tunnel profile, keep it and the existing ChatGPT app. Stop the old `tunnel-client run` with **Ctrl+C in its terminal once** before switching to MoHuddle's managed connection. If you previously used the official client's managed runtime instead, stop that alias with `tunnel-client runtimes stop mohuddle-chatgpt`. MoHuddle does not take over or kill separately started tunnels.

Build and install the updated executable, then start or resume the room:

```bash
make install
mohuddle
```

In MoHuddle:

```text
/join @chatgpt
```

MoHuddle enables the private project manager, reads your existing tunnel profile, and starts one `tunnel-client` in the background. No second terminal or repeated `init --force` is needed. Ask ChatGPT to list rooms and join one by name when the **CHATGPT row at the bottom of the agent list** says the tunnel is ready. The row shows connecting, waiting for ChatGPT, connected, paused, disconnected, or failure; transport readiness alone does not mean the website has joined.

A confirmed join is shown as **connected** even if the periodic tunnel health check is still pending. A later tunnel failure or recovery remains visible alongside the participation state; an old participation lease is not proof that the tunnel is still working.

Access lasts eight hours by default. `/chatgpt on 30m` chooses a shorter lifetime when enabling new access; the supported range is one minute to 24 hours. Repeating `/join @chatgpt` or `/chatgpt on` preserves an active grant, conversation, pause, and exchange budget. Use `/chatgpt renew 30m` when you explicitly want to replace an existing grant and require a new join.

```text
/chatgpt status       inspect access, tunnel health, expiry, and pause
/chatgpt restart      cycle the owned tunnel and stdio bridge
/chatgpt resume       resume posting and authorize more exchanges
/leave @chatgpt       revoke this room's access and disable its auto-connect
```

Restart repairs the shared transport for every room without cancelling local agent work or renewing room authorization. Unexpected process exits and sustained failed health checks get at most three automatic restarts, with backoff; exhaustion requires `/chatgpt restart` or another explicit join. Health checks require the owned process, the tunnel's health/readiness endpoints, and a successful control-plane poll. They cannot guarantee that a particular ChatGPT request or website turn will succeed.

Switching the displayed room keeps other room runtimes, monitors, and panels active. Closing one room revokes only that room; quitting MoHuddle stops all rooms and the shared tunnel. Project grant expiry stops the managed tunnel. One manager owns a tunnel profile; multiple rooms use it through that manager. A separate MoHuddle process cannot take over its manager socket or tunnel. MoHuddle reuses control-plane settings and credential references without overwriting the source profile, keeps generated runtime files private, and restricts the health listener to a random loopback port. Raw tunnel logs are not posted to the room or saved by the supervisor; failures use safe diagnostic descriptions.

For automatic startup in this particular room, explicitly opt in with `/chatgpt auto on`. Opening that room enables its access and ensures the shared tunnel is running with an eight-hour project grant. `/chatgpt auto off` disables that preference without interrupting current access; `/leave @chatgpt` disables it and revokes access. This preference is local to this user's room/state directory. ChatGPT still needs to join/read from its website conversation, and live follow-ups default ON once it joins; explicit pauses remain saved separately.

If your existing profile has another name, select it once with `/chatgpt profile NAME`. The default is `mohuddle-chatgpt`, using the official client's profile directory (`TUNNEL_CLIENT_PROFILE_DIR`, otherwise `$XDG_CONFIG_HOME/tunnel-client` or `~/.config/tunnel-client`).

## Independent rooms through one connection

A new ChatGPT conversation offers the current project's existing rooms, such as
**Room 1** and **Room 2**, plus **Create new room**. It does not pick silently,
even when there is only one room. You can say **“Join Room 2”** or **“Create a new
room for this conversation.”** The panel offers the same choices. Room names are
automatic, case-insensitive, permanent, and never reused after deletion. Existing
internal IDs remain valid. No extra tunnel profile or ChatGPT connection is needed.

`mohuddle_rooms` returns names, objectives, and availability. `mohuddle_join` takes
an optional `room` selector. Without one, a known conversation resumes its saved
selection; a new conversation receives `selection_required`. Creation uses
`mohuddle_create_room` with an `operation_id` retained for retries, preventing
accidental duplicate rooms. New rooms use the current project and the host's
saved default AI team, with separate native provider sessions. Existing rooms
retain their history, permissions, settings, and provider sessions.

Keep the returned `room_id` and `participation_id` together. Each conversation
has one active attachment, and each room permits one active ChatGPT coordinator.
An occupied room cannot be taken over by repeated joins. An explicit switch
detaches the old panel while already accepted work continues. A room's cursors,
coordination run, handoffs, pauses, notification budget, and exchange budget stay
with that room. Switching the terminal view never redirects a ChatGPT attachment.

Only rooms in the startup project are listed or accepted through the manager.
Local `/rooms` can show the broader saved-room catalog. `/new` and `/resume room2`
change the terminal view while other opened rooms keep running; `/rooms` shows
status every five seconds. `/rooms close room2` closes a background room and
blocks automatic rejoining until the host opens and enables it again. `/quit`
stops the whole manager. Saved rooms are opened on demand after restarting the
app; they are not all restarted automatically.

Provider concurrency is shared across managed rooms, with queued rooms taking
turns. A second room does not create additional provider capacity. Writers in the
same canonical workspace serialize; read-only work can continue within available
capacity. Background approvals remain pending and reappear when that room is
selected. The manager keeps draining room events even when its terminal view is
elsewhere, so a full UI event buffer does not stop background work.

After upgrading at a safe point, restart MoHuddle and its bridge, keep the same
tunnel and ChatGPT connection, refresh its tool definitions, and ask ChatGPT to
list rooms. Verify the two new tools and the join `room` field before reporting
`rooms-v1` compatibility. Actual website follow-ups still depend on the open
ChatGPT panel and host acceptance; a healthy manager is not proof of delivery.

## One-time tunnel profile setup

Skip this section if your profile and ChatGPT app already work. Complete this setup before the first managed join on a new installation.

Open [OpenAI Platform tunnel settings](https://platform.openai.com/settings/organization/tunnels) and create a tunnel. Associate it with the ChatGPT account/workspace that will use the room. Tunnel creation requires **Read + Manage**; selecting and running it requires **Read + Use**. ChatGPT developer-mode access is a separate permission. The [official permission and workspace instructions](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels#permissions-and-access) describe account-specific requirements.

Install the official `tunnel-client` using the download offered there or its [release page](https://github.com/openai/tunnel-client/releases/latest). Obtain a runtime API key for the tunnel, then enter it privately in your Bash terminal:

```bash
read -r -s -p 'Tunnel runtime API key: ' CONTROL_PLANE_API_KEY
export CONTROL_PLANE_API_KEY
```

Create a named stdio profile, replacing the tunnel ID and executable path. For an executable path with spaces, retain the single quotes inside the command string:

```bash
tunnel-client init \
  --sample sample_mcp_stdio_local \
  --profile mohuddle-chatgpt \
  --tunnel-id tunnel_REPLACE_WITH_YOUR_ID \
  --mcp-command "'/absolute/path/to/mohuddle' chatgpt serve"

tunnel-client doctor --profile mohuddle-chatgpt --explain
mohuddle
```

Run `/join @chatgpt` in MoHuddle. The executable can be found with `command -v mohuddle` after installation; managed connections always use the running MoHuddle executable and pin its current room internally.

The profile's `env:CONTROL_PLANE_API_KEY` reference must be available in the environment **where MoHuddle starts**, not just another terminal. Alternatively, configure the profile's `control_plane.api_key` as `file:/absolute/private/key-file` and keep that nonempty file mode `0600`. MoHuddle retains the reference and never copies the key into settings, room messages, or process arguments. Do not paste an API key into a room command. No MoHuddle administrator credential is used.

Limit the tunnel's associations and app access to the people who should see this room. Anyone authorized to use this particular tunnel/app can list, create, and attempt to join rooms in the locally enabled project; the grant is not tied to an individual OpenAI user. Each room permits one active ChatGPT coordinator. Other conversations can use other rooms.

## Connect it in ChatGPT

Enable developer mode under **Settings → Security and login**, if it is available for your account. Open [ChatGPT Plugins](https://chatgpt.com/plugins), use the plus button to create a developer-mode app, select **Tunnel** under **Connection**, and select or paste your tunnel ID. Use the private tunnel connection rather than a URL. See [OpenAI's connection instructions](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels#connect-from-chatgpt).

Set **Authentication** to **No Authentication** in the ChatGPT app form. This stdio bridge does not implement an OAuth server, so selecting OAuth produces an “MCP server … does not implement OAuth” error. ChatGPT supports this mode in its [developer-mode authentication options](https://developers.openai.com/api/docs/guides/developer-mode). Keep the existing tunnel selected when correcting this setting.

This setting skips an additional OAuth login to MoHuddle. OpenAI still controls access to the private tunnel through its organization/workspace permissions, and MoHuddle validates the local project grant and the selected room's participation capability on requests. The grant is held by the bridge and does not distinguish between OpenAI users who can use that tunnel. Anyone authorized to use this tunnel/app can attempt to claim an available ChatGPT seat in an eligible project room and read that room. Keep the app private and restrict tunnel access to the intended users; a workspace association alone does not establish that only you have access.

Add the MoHuddle app to a ChatGPT conversation. A useful first message is:

> Join my MoHuddle room and open its live panel. Keep our side conversation private. Share only results intended for the room, ask the other participants for feedback when useful, and follow up only when you have something substantive to add.

ChatGPT calls `mohuddle_join`, retains the returned participation ID, and can open `mohuddle_panel`. The MoHuddle roster shows when it is connected. In MoHuddle, address it with:

```text
@chatgpt What conclusions can you draw from the other participants' findings?
```

`/ask @chatgpt MESSAGE` also works. ChatGPT is a conversational peer; its website session is separate from the room's Codex CLI participant. It can assign work to those local participants through `mohuddle_request_work`; their existing permissions and the room's current Default/Plan mode control execution. ChatGPT itself has no filesystem or approval controls.

## Posting, requesting replies, rounds, and work

| Intent in ChatGPT | MCP call |
| --- | --- |
| Share a result | `mohuddle_publish` with `text` and no `request_replies`. This only posts text. |
| Get Codex's answer or review | `mohuddle_publish` with `request_replies: ["codex"]`, then `mohuddle_read`. Replies are read-only. |
| Hold a moderated discussion or review | `mohuddle_request_round` with the existing proposal in `text`, optionally `participants`, then `mohuddle_read`. The normal room moderator synthesizes last; every turn is read-only. |
| Have Codex make an authorized edit | `mohuddle_request_work` with `target: "codex"` and a complete task in `text`, then `mohuddle_read` for status/results. |

Writing `@CODEX` or `@codex` in message text does not schedule a reply or work. The plugin does not execute composer commands. Requests starting with `/ask`, `/round`, or `/delegate` are rejected before posting or dispatch, with an error naming the correct tool. Quoted commands and command names mentioned within ordinary prose can still be discussed.

Multiple `request_replies` recipients receive independent read-only turns, which may run concurrently. A native round gives participants the floor sequentially and puts the moderator last, regardless of role assignments written in the message. A round waits for existing work/replies to finish before it can be requested. It does not automatically create another review stage or start implementation.

For “draft, review, then apply,” ChatGPT must obtain and read the actual draft first, request review of that exact material with its message sequence in `reply_to`, read the reviews, and only then request authorized implementation if the user's conditions have been met. A single ChatGPT turn may contain several ordered tool calls. A single posted paragraph cannot create those scheduler dependencies. Completion does not prove consensus, and silence or a failed review is not agreement.

For example, tell ChatGPT: “Ask Codex to apply that title-only edit, preserve the body and unrelated changes, check the diff, and report back. No commit or push.” ChatGPT should call `mohuddle_request_work`, preserving those constraints. You do not need to repeat that instruction in MoHuddle. Work queues behind an active workspace writer and uses the participant's existing permissions. If the room is in Plan mode, it remains read-only until normal host approval.

Receipts identify the `action` (`post`, `replies`, `round`, or `work`) and explain the `next_action`. Separate `message_posted`, `agent_scheduled`, and `scheduled_agents` fields distinguish a saved message from dispatch. Queued work counts as scheduled, not completed. Work and rounds also return `workflow_id` and `work_state`; round receipts name the moderator and actual floor order. `mohuddle_read` exposes recent `work` status with its `kind` and matching message workflow IDs. Pending `replies` and terminal `reply_results` are matched to their original `source_sequence`, including failed or cancelled requests.

Failures include `failure_code` and `failure_reason`. A partially saved request may report `message_posted: true` and `agent_scheduled: false`. If a connection breaks before confirmation, `outcome_unknown: true` and nullable outcome fields prevent a false claim of success or failure. Retain the original `operation_id`, reconnect/read status, and retry identical content instead of creating duplicate work.

The [MoHuddle usage skill](../internal/chatgpt/skills/mohuddle-room/SKILL.md) is embedded in the MCP server's initialization instructions, preceded by a concise operating contract. Join, read, and panel results also return current `usage` guidance, and panel notifications reinforce the same sequence. This covers routing, dependencies, permissions, receipts, and recovery without requiring users to teach the protocol in each conversation. Tool descriptions and server checks reinforce the guidance; instructions alone cannot guarantee every model decision.

## Effort for each task

ChatGPT can select effort without changing the room's standing `/effort`. Use `effort` on `mohuddle_request_work`; use an `efforts` map on `mohuddle_publish` with `request_replies` or on `mohuddle_request_round`. A round can give its moderator a separate level. Optional `effort_reason` is shared and limited to 512 UTF-8 bytes.

After joining and reading capabilities, a small authorized edit can use:

```json
{
  "participation_id": "FROM_JOIN",
  "operation_id": "title-edit-1",
  "target": "codex",
  "text": "Apply the approved title-only edit and verify the diff.",
  "effort": "low",
  "effort_reason": "Mechanical edit with a narrow scope"
}
```

For independent replies, add `"efforts": {"codex": "low", "claude": "medium"}` alongside `"request_replies": ["codex", "claude"]`. Map keys must name selected participants, including the moderator for a round. Auxiliary names such as `codex-1` work the same way. Text-only posts cannot carry effort choices.

Join, read, and panel results expose `effort_capabilities` and `moderator`. Each participant lists its model, `standing_effort`, and `available_efforts`. `capability_source: "model_catalog"` means the provider reported model-specific support; `"provider_validation"` means only the adapter's accepted levels are known. Discovery runs in the background so reads stay available. Queued choices are revalidated against the model at dispatch; incompatible choices fail visibly without a more expensive substitute.

The embedded instructions recommend supported `low` for mechanical tasks, `medium` for ordinary implementation or review, and `high` for difficult debugging or architecture. Higher levels require an explicit human request in this guidance. Fresh connections receive these instructions naturally through tool descriptions, initialization, and room reads. This is behavioral guidance, not an enforced spending cap.

Omission preserves standing room behavior, including a standing `max`. Explicit `auto` requests provider default, which may itself be expensive. Choices persist through the accepted operation's continuations and supported restart recovery. Other participants retain their settings; the next unrelated task returns to its standing setting. Codex retains its native thread when changing effort. Adapters that reset their sessions receive the necessary transcript again. Returning to default clears the temporary native override; if Codex cannot resolve its default, it reports an effort error before starting another turn.

Receipts return accepted `efforts` and `effort_reason`. Work and reply status retain these and `effort_status`, separating requested, applied, and provider-reported settings. Applied effort is what MoHuddle sent; empty `reported_effort` means **unconfirmed**, even when a task succeeded. The panel and local turn details show this distinction. Providers do not all report effort or token totals, so evaluate savings alongside result quality.

An identical operation ID must retain its original effort and reason. Changing either requires a new ID; changing effort cannot bypass repetition or exchange limits. Existing permissions and Plan mode still apply.

After upgrading, restart the room and tunnel at a safe idle point, refresh the app's saved tool metadata in ChatGPT, and rejoin. The bridge now registers fourteen tools: verify the new `effort` and `efforts` input fields and `effort_capabilities` in the room view. A new conversation alone does not refresh cached schemas. See the [implementation plan](plans/chatgpt-effort-management.md) for scope and validation.

## Side conversations and ongoing participation

Joining opens the live panel with automatic follow-ups **ON by default**. No separate Enable click is needed. The setting is saved in the room and shared by all panels. You can read the room while having a private side conversation in ChatGPT, then ask ChatGPT to publish a selected result. Publishing is the explicit sharing boundary, so avoid asking it to post your whole private conversation.

The panel automatically requests a ChatGPT turn when relevant human/peer updates arrive, subject to the saved pause and shared allowance. ChatGPT reads those messages and decides whether a useful contribution is warranted. For a monitored run, the panel can notify about a ready handoff while independent peers are working. Older servers wait for pending work to finish. It ignores ChatGPT's own posts, and defaults to **32 automatic notifications over 60 minutes**, with at least 20 seconds between them. Manual reviews do not spend this notification budget. Room exchange and panel notification budgets are separate; their remaining counts and pause reasons are displayed.

The host can save different limits for this room:

```text
/chatgpt limits                 show this room's settings
/chatgpt limits exchanges 64    exchanges per host authorization (1–1000)
/chatgpt limits followups 64    automatic notifications per shared allowance (1–1000)
/chatgpt limits duration 2h     panel session duration (1m–24h, whole seconds)
/chatgpt limits repeats 4       identical requests without progress before pausing (2–20)
/chatgpt limits reset           restore defaults: 32 exchanges, 32 notifications, 1h, 3 repeats
```

Settings are local to the user, state directory, and room; they persist across room restarts. Legacy configurations use the new defaults. Saving a limit preserves usage already spent and any explicit host pause. Panel changes apply on the next update, measured from the saved allowance start. Reopening, reconnecting, access renewal, and room restart do not replenish notifications. `/chatgpt resume` refreshes the exchange budget and clears a repetition pause. **Resume live follow-ups** or `/chatgpt followups on` clears only the saved follow-up pause; **Renew follow-up allowance** or `/chatgpt followups renew` explicitly resets the shared notification count and duration. Neither control grants new task or filesystem authority.

After three identical requests without observable progress, the next new request pauses dispatch by default. Changing an operation ID, reply reference, or whitespace does not evade the check; an identical retry using its original operation ID does not consume another exchange or repeat. Distinct public peer text or a newly completed ChatGPT work/round request counts as observable progress. Repeated answer text, failures, polling, and ChatGPT's own posts do not. This is a repetition heuristic, not a judgment that a result is correct or that a task is complete. A new local human message or `/chatgpt resume` clears the pause. Budget and repetition pauses leave accepted work/replies running, within their normal deadlines; reads and summary posts remain available. The panel continues refreshing results while automatic notifications are paused, and **Ask ChatGPT to review updates** can request a manual review.

Use **Pause live follow-ups** or `/chatgpt followups off` when you want to pause automatic updates. The pause applies to every panel and persists until you explicitly resume; merely discussing status does not pause the authorized task. It prevents further automatic notifications; a turn already requested from ChatGPT may still need to be stopped in the ChatGPT UI. **Ask ChatGPT to review updates** requests one review manually.

ChatGPT controls tool approvals and whether component notifications start a new model turn. A panel cannot guarantee background execution when the chat is closed, suspended, or the host declines a request. If the host does not support follow-ups, use the ChatGPT composer to ask it to read the room. During an active turn, `mohuddle_read` can wait up to 25 seconds for peer replies. This integration does not automate the ChatGPT browser or run a separate API model behind your website conversation.

## Room controls

| In MoHuddle | Effect |
| --- | --- |
| `/join @chatgpt` | Enable access and start/repair the background tunnel, preserving an existing valid grant. |
| `/chatgpt on 30m` | Enable new access with a chosen lifetime; preserve an existing grant. |
| `/chatgpt renew 30m` | Explicitly rotate the grant, requiring ChatGPT to join again. |
| `/chatgpt status` | Show tunnel health, profile, retry count, connection, expiry, pause, and exchange budget. |
| `/chatgpt restart` | Restart the owned tunnel and bridge, preserving room work and authorization. |
| `/chatgpt profile NAME` | Select an existing tunnel profile; use join/restart to apply. |
| `/chatgpt auto on\|off` | Remember or disable a fresh eight-hour connection whenever this room opens. |
| `/chatgpt manual [duration]` | Show an explicit single-room bridge command for external supervision. Preserve an existing shared manager tunnel. |
| `/stop` | Pause ChatGPT posting and cancel pending peer replies along with other room work. |
| `/chatgpt resume` | Resume posting, clear a repetition pause, and refresh the configured exchange budget (32 by default). |
| `/chatgpt limits [...]` | Inspect or save this room's exchange, notification, duration, and repetition settings. |
| `/leave @chatgpt` or `/chatgpt off` | Revoke this room's grant, delete its private file, cancel pending peer replies, and disable its auto-connect. Other rooms keep working. |
| `/leave @all` | Revoke ChatGPT access and remove local participants. |

Work already accepted by the scheduler follows the normal room lifecycle even if ChatGPT leaves, loses its lease, or its grant is revoked. Use Esc or `/stop` in MoHuddle to cancel running and queued work.

ChatGPT can also call `mohuddle_leave`; this releases its seat and cancels pending replies while leaving the local grant enabled. A participation lease lasts two minutes and is renewed by reads, including panel refreshes. Accepted queued and running peer replies continue after passive lease expiry or a polling gap, within their existing deadlines and while the room grant remains valid. Rejoin to submit new requests or collect results after the participation lease expires; rejoining does not cancel or duplicate accepted replies. Explicit leave, host stop, grant revocation/rotation, grant expiry, and room closure still stop pending replies. A second conversation cannot take over an active lease.

Room grant rotation, explicit room closure, and app restart invalidate that room's participation. The stdio bridge validates its private manager connection file on each request; extending the manager lifetime preserves other rooms' attachments. Ask ChatGPT to rejoin after its room's grant is rotated or the app restarts. Missing, expired, or unsafe connection files block access immediately. The manager is pinned to the startup project's canonical workspace and private socket. ChatGPT switches eligible rooms by name without restarting the tunnel. `mohuddle chatgpt serve --room room2` or `--connection FILE` still selects a private single-room bridge; automatic discovery prefers a running manager. `/join @all` only joins configured local providers; granting ChatGPT access is explicit.

## Security boundaries and limits

- A fresh 256-bit grant is stored in a `0600` file beneath a `0700` directory. The host retains its hash in memory. Expiry and revocation are checked for every request, including already-connected clients. Grants are not persisted in room state or returned to ChatGPT.
- The bridge accepts only a private Unix socket and a private regular connection file. It refuses ordinary MoHuddle administrator credentials. Its stdio protocol has bounded frames; it has no network listening option.
- ChatGPT can call only join, read, read_reply_draft, publish, request_work, request_round, and leave for the granted room. The work endpoint accepts one local participant and task text, using the normal scheduler, workspace write lease, permission ceiling, and approvals. Rounds use that scheduler with a read-only permission ceiling and the native round runner. Generic API history, room controls, approvals, filesystem grants, and command invocation remain denied regardless of assigned scopes.
- Room output exposes shared human/AI text, authors, sequence references, and bounded reply/work status. Terminal reply results include a safe `reason_code`, a recorded `completed_at` when available, the `answer_sequence`, and `has_partial_response`. Older records without a recorded cause report `unknown`; timestamps are not invented. Partial public drafts remain in local Turn details (Alt+T). Ordinary reads return `draft_available`; `mohuddle_read_reply_draft` retrieves only sanitized drafts linked to an unsuccessful ChatGPT reply whose source message has been read. Drafts are incomplete, may be truncated or evicted, and never count as a successful answer. Tools, reasoning, and unrelated turn history remain private. Room output excludes tool logs, attachments, internal errors, provider session IDs, socket paths, and credentials. Existing room text is visible after joining, including text humans or peers have already placed in the shared transcript; there is no automatic redaction of secrets someone posts as message text.
- A contribution is AI-authored data. Command-looking text cannot invoke a command. Peer replies use ephemeral read-only turns. Local agents configured as `full` keep full-machine filesystem read access during those turns, with writes blocked; no extra grant is needed to inspect a neighboring repository. AGY inspection requires the OS write sandbox described in the [permission profiles](../README.md#filesystem-access-and-approvals). Their control fields cannot authorize work, change the roster, or update correction records. Provider approval requests during those turns are automatically denied. Read-only peer replies cannot promote themselves into work. An explicit `mohuddle_request_work` call can assign the user's requested task while preserving ChatGPT authorship.
- Publishing and work requests are idempotent when retried with the same operation ID and identical action/content. Limits are 16,000 bytes per request, 20 new posts/work requests per minute, four pending peer replies, four unfinished work requests, and the room's configured exchange budget (32 by default) per host authorization. Each new peer-reply request, work request, or moderated round consumes one exchange, even if an accepted request later fails. Reading and text-only posts do not consume exchanges. A new local human message or `/chatgpt resume` refreshes the budget. ChatGPT cannot change these host settings. Read pages are bounded to 100 messages, with individual long messages shortened.
- The panel has no external scripts or network destinations. Room text is rendered as text. Its notifications contain fixed instructions and cursors, not copied room content. Follow-ups are opt-in and bounded.
- The OS account running MoHuddle and `tunnel-client` is trusted. Keep its state directory private. The tunnel and room grants do not isolate other processes running as that same OS user.

## Troubleshooting

- **Connecting or recovering:** allow the first successful control-plane poll (normally up to a poll interval). Use `/chatgpt status` for details. After sustained failure MoHuddle restarts up to three times; `/chatgpt restart` retries manually. `/chatgpt resume` changes participation authorization, not networking.
- **A separately started tunnel is already running:** stop that process in its original terminal, then `/chatgpt restart`. Detection uses known profile/runtime health endpoints and, on Linux, process arguments; it cannot identify every custom externally managed launch. Keep one owner per tunnel. Use `/chatgpt manual` if you prefer to keep your external supervisor.
- **Missing environment variable or key reference:** export the named variable before starting MoHuddle, or configure a private `file:` reference in the profile. Existing processes do not receive environment changes made in another shell.
- **Tunnel unavailable in ChatGPT:** check developer-mode access, the tunnel's ChatGPT workspace association, and Tunnels Read + Use permissions. A Platform organization association alone may be insufficient.
- **“MCP server … does not implement OAuth”:** select **No Authentication** in the ChatGPT app form for this private stdio connection. If that option is selected and the error persists, record the exact error and selected settings before changing the working tunnel configuration.
- **Invalid/expired connection:** enable ChatGPT in the MoHuddle room, then ask ChatGPT to join again. Current builds reload renewed grants for the same room automatically. If an older bridge is still running, restart the tunnel once after upgrading MoHuddle.
- **Another conversation is participating:** leave that conversation's MoHuddle session, close its panel and wait two minutes, or explicitly rotate with `/chatgpt renew`. Repeated joins and tunnel restarts do not steal the seat.
- **Paused, exchange limit reached, or repeated requests without progress:** inspect the reported cause and available results, then use `/chatgpt resume` in MoHuddle. Follow-ups resume automatically if their saved setting is ON and the shared allowance remains available. `/chatgpt limits` shows the saved settings. A notification/time limit requires **Renew follow-up allowance** or `/chatgpt followups renew`; opening a new panel cannot reset it. This does not replenish the room exchange budget.
- **Peer reply pending:** only selected, present local peers are eligible. They may be waiting for provider capacity. Leaving/revoking ChatGPT cancels those requests.
- **Posted but nobody responded:** inspect `action` and `agent_scheduled`. For an answer, use `request_replies`; for a moderated round, use `mohuddle_request_round`; for edits, use `mohuddle_request_work`. Mentions and command text do not dispatch.
- **A message describes several future stages:** only the explicit tool operation runs. Read its completed result and issue a separate tool call for each dependent stage. Do not infer that a published plan is running.
- **Work queued:** read its `work` state; it may be waiting for the workspace writer or provider capacity. Do not resubmit with a new operation ID. Any required approval remains in MoHuddle.
- **A tool is absent after upgrading:** restart the MoHuddle room and tunnel process to load the new binary and schema, then rejoin the resumed room. Update the connection's saved metadata using the controls available in ChatGPT's plugin menu and start a new conversation if necessary; a new chat alone does not rescan the server. Verify fourteen registered tools, including `mohuddle_rooms`, `mohuddle_create_room`, `mohuddle_read_reply_draft`, `mohuddle_request_round`, and `mohuddle_request_work`. Do not substitute command text for a missing tool. Existing grants are process-local.
- **No automatic turn:** the website may require a tool approval or decline component follow-ups. Ask in the ChatGPT composer to read the room and contribute if useful. Keep both the room and tunnel client running.

## Validation

Lifecycle tests exercise idempotent joins, preserved pauses/budgets, serialized process replacement, bounded retries, cancellation during startup/backoff, grant expiry, exclusive ownership, process-group cleanup, safe configuration/diagnostics, per-room startup opt-in, and roster states. An optional test validates generated configuration with an installed official `tunnel-client doctor`, using a dummy key and no running tunnel.

Manager integration tests use independent MCP clients to cover selection, duplicate-creation retries, occupied rooms, explicit switching, reconnects, restart recovery, per-room policies, and preservation of the gateway when one room closes. Scheduler tests cover shared capacity, writer serialization, canonical workspace aliases, and background event draining.

`make check` covers grant isolation, bounded history, retries, cancellation, peer permissions, writable work dispatch, write-lease queuing, approval routing, MCP tool discovery and round trips, and a subprocess running the real stdio command. The embedded panel tests use Node and also run explicitly in CI. Run them separately with `node --test internal/chatgpt/panel_test.mjs`.

Connecting an actual ChatGPT account requires its tunnel, runtime key, and account-side app setup. Local tests do not prove account entitlement or website follow-up behavior.

## Recover an interrupted reply

For `event_queue_overflow`, MoHuddle's local response queue reached its bounded capacity. Inspect `reply_results` and call `mohuddle_read_reply_draft` when `draft_available` is true. Supply `participation_id` and `reply_id`; preserve the returned `draft_id` and page with `segment: next_segment` and `offset: next_offset` while `has_more` is true. Pages default to 8,000 characters (maximum 16,000). `incomplete` is always true; `capture_truncated: null` means an older capture did not record whether text was truncated. Each segment is a provisional draft, not a completed answer.

Recovery reads saved public output without another model call, posting a response, writing a file, or spending an exchange. Existing limits retain at most 40 turns and 512 KiB per room, with at most 64 KiB of draft text per turn. Older drafts require a verified conversation/message link; missing or ambiguous links return `draft_unavailable`. Refresh the ChatGPT app's tool metadata after upgrading to discover the eighth tool.

## Coordination monitoring and reliable result delivery

Monitoring is opt-in. It tracks handoffs; only explicitly registered read-only continuations can schedule a future stage. Enable it locally for a run in
which ChatGPT is expected to coordinate multiple stages:

```text
/chatgpt monitor start
/chatgpt monitor status
/chatgpt monitor stop
/chatgpt monitor resume
```

Only the local host can start or resume a monitored run. A run has a durable ID
and survives room restart independently of the access grant. `stop` suppresses
its follow-ups and further ChatGPT work dispatch; already accepted jobs follow
normal room lifecycle. Use `/stop` to cancel those jobs as well. `/stop` also
stops the monitor. Access renewal, `/chatgpt resume`, rejoining, panel refresh and
restart do not resume a stopped monitor. Explicit monitor resumption rotates its
ID, rejecting delayed reports from the old run. Monitoring grants no new task,
filesystem or Jira authority.

The room scheduler reconciles monitoring every second without a website panel;
the local TUI refreshes its display every five seconds. An unresolved ready handoff
or a pending run with no queued/running jobs for three minutes displays a
coordinator-action warning. Other running jobs do not hide an independent ready handoff. Resource waits are shown separately. One hour without
a successful operation completion displays a diagnostic, including when jobs
remain active. Acknowledgements, polling, text posts and failed operations do not
reset the success clock. Blocked, complete and stopped runs do not raise these
warnings. These measure operations, not vetted backlog items.

`mohuddle_read` includes `coordination`: the current run, summary, elapsed times,
pending/waiting counts, durable handoffs, registered continuations, delivery status,
latency counters, and the most recent 20 events from a bounded 200-event history.
Unresolved handoffs and notification claims survive event-history eviction. The panel and `/chatgpt status` distinguish actual result availability,
notification attempts, panel-reported website acceptance/rejection/unknown,
explicit coordinator acknowledgement and subsequent accepted assignments.
Website acceptance does not prove another model turn happened. Missing evidence
stays unknown; closing the panel prevents further panel delivery observations.

ChatGPT uses `mohuddle_coordinator_report` to acknowledge a retained `result_id`
and report `pending` with its next action, `blocked` with a reason, `complete`, or
`stopped`. Completion is the coordinator's report, not an independent verdict.
Status-only reports omit `result_id`; routine panel reads never acknowledge.
Reports require the current run ID and an event ID reused only for identical
retries. `mohuddle_notification` is an app-only telemetry tool and never records
coordinator acknowledgement. Neither tool consumes an exchange or dispatches an
agent. A watchdog does not automatically send repeated continuation prompts.

For substantial independent investigation or review, set `reply_class` to
`research` in `mohuddle_publish` with `request_replies`. Its deadline is thirty
minutes; omission or `quick` retains ten minutes. Time includes scheduling waits.
The class, actual deadline and requested/applied effort are returned with reply
status. Choose supported effort explicitly; a mechanical relay rarely warrants
maximum reasoning effort. Identical operation retries reuse the original job;
changing its class is a conflicting retry, not a deadline extension.

If a message is shortened, use `mohuddle_read_message` with its delivered
sequence. Pages default to 4000 Unicode characters (maximum 8000); follow
`next_offset` while `has_more`, retaining `sha256`. The hash identifies the
sanitized shared text, not an arbitrary source file. `complete` means the last
page of that stored message, not proof the agent completed its task. Private tool
output and arbitrary files are not accessible. A missing retained message returns
`unavailable`, never an invented empty success.

Failed replies with `draft_available` use the existing
`mohuddle_read_reply_draft`; drafts remain explicitly incomplete, may contain
replacement segments, and may themselves be capture-truncated. If partial output
was captured but evicted from retained turn history, status says recovery is
unavailable. Neither retrieval tool reruns research or grants approval.

After upgrading, refresh the app's tool metadata and rejoin at a safe point.
There are fourteen tools, including app-only notification telemetry and persistent follow-up controls. Older clients
that omit the new class keep their previous deadlines, and old rooms begin with
monitoring disabled. See [the retained roadmap](plans/coordination-reliability.md)
for deferred automation, provider repair and workflow improvements.


## Continuous coordination instructions and handoffs

MoHuddle owns the coordinator guidance: the embedded skill, full policy on join,
short reminder on every read, participant handoff instructions, and panel prompts
share a versioned source. Join/read advertise `instruction_version` and
`capabilities`. A separate personal ChatGPT skill is optional. After installing
an updated host/bridge, rejoin and verify the advertised version; initialization
instructions alone may remain cached by the website.

For a monitored run, ChatGPT records the objective, authorized scope and completion
criteria with `mohuddle_coordinator_report`. It records a next action and owner,
then supplies `coordination: {run_id, handoff_id}` on the actual scheduling call.
Acknowledgment or a promise to dispatch leaves the handoff outstanding. An accepted
linked assignment resolves it. Status questions preserve the existing objective;
explicit pauses and scope changes still apply. These summaries are coordinator
reports, not independent authority or proof of successful completion.

Use `handoff_only: true` with `result_id` to block or complete one branch while
independent work continues. Use `waiting_on: "human"` with the decision owner for a human decision, `external` for an external dependency, or `assignment` with `waiting_for` for active work. A blocked handoff stays outstanding without being labeled stalled. A linked next assignment resolves only that handoff; returning the overall run to pending does not reopen other blocked branches. `waiting_for` must identify a real active workflow or
reply; when it ends, that handoff becomes actionable again. Without `handoff_only`,
blocked/complete/stopped describe the whole monitored run. Completed runs cannot
retain pending jobs or registered reviews. A report of stopped prevents new work;
local `/stop` cancels already running jobs as well.

The first automatic handoff notification is eligible immediately, with reminders
at 60 and 180 seconds after the result becomes ready. At most three automatic
attempts are allowed per handoff, with 20 seconds between notifications across
panels, within the shared room follow-up allowance. Claims are recorded before
website delivery. Host acceptance does not clear the handoff; explicit host
rejection stops automatic retries. Unknown delivery stays unknown and remains
eligible for bounded recovery. A panel reports enabled, hidden, unsupported,
manual, or budget-limited delivery; observations older than 45 seconds become
unknown. Closing/suspending ChatGPT cannot be overcome by a prompt.

### One registered read-only continuation

When `registered_readonly_continuation_v1` is advertised and the user has already
authorized a writer followed by review, a work request can include:

```json
{
  "coordination": {
    "run_id": "CURRENT_RUN",
    "continuation": {
      "target": "claude",
      "text": "Independently review the resulting change against the requested behavior; return findings and evidence. Do not edit files.",
      "reply_class": "research",
      "effort": "medium"
    }
  }
}
```

Choose an actual present reviewer distinct from the writer and a supported effort.
Two exchanges are reserved when the parent is accepted. The review runs through
the existing read-only reply scheduler after successful parent completion, with
the complete retained parent result attached. It can wait for reply/provider
capacity. Passive participation expiry does not cancel it. There is no automatic
interpretation of a review as approval and no chained writable continuation.

Missing or oversized results, failed parents, unavailable reviewers and ambiguous
dispatch become visible coordinator actions. Original grant revocation/expiry,
explicit leave, host stop, or host restart cancels unstarted continuations; renewal
does not revive them. A durable dispatch identifier prevents duplicate review
assignments after retries. Inspect `coordination.continuations` before requesting
another review.

The work receipt also returns the registered `continuation` and directs the
coordinator to inspect its state. A workflow resumed after an error receives a
new result handoff; the earlier failure does not hide the recovered output.

Existing rooms load with optional fields absent and no inferred continuations.
Percent-complete estimates are not introduced. The panel instead shows assignment,
last observed activity, stale status, handoff age, owner, delivery and next action.
See [coordination validation](coordination-validation.md) for checks and rollout.


## Upgrade and verify automatic follow-ups

Restart the host and bridge with the new version at an idle point. In ChatGPT,
refresh the developer connection's tools and use a new conversation when needed.
Join once: the room card should render automatically and show **saved setting ON**
and **On and connected**, unless you explicitly paused or a limit was reached.
The initial allowance begins when an enabled panel first reports availability.
Existing rooms without a saved preference default ON; explicit host stops and
stopped monitored runs remain effective. Known old notification consumption is
carried forward. An old panel's `manual` observation cannot distinguish its former
default from a deliberate click, so it is not migrated as a saved OFF preference.

In the card's **Connection readiness** details, check `server_version`,
`tool_contract_version` (`rooms-v1`), and `instruction_version`. Ask ChatGPT
to inspect its actual input definitions for `coordination.continuation`,
`handoff_only`, `waiting_on`, and `client_contract_version`. After that inspection,
also verify `mohuddle_rooms`, `mohuddle_create_room`, and the `room` input on join. It can then report `client_contract_version: "rooms-v1"` on a read. Until then,
client compatibility is unknown. The report is labeled as client-reported;
a successful server read cannot prove that cached tool definitions were refreshed.
If fields remain absent after Refresh and a new chat, create a fresh developer
connection to the same tunnel and select it, preserving the old connection as a
fallback until the new one works. Do not rerun business tasks as a refresh test.

`/chatgpt followups status` shows saved policy and remaining shared allowance.
Monitoring shows panel unavailability, rejection, and exhausted per-handoff retries
separately from coordinator action. A website-accepted notification does not prove
ChatGPT read a result or dispatched work. Closed or suspended website panels cannot
be awakened by room prompts; ChatGPT can reopen one during an active turn.

The additive saved fields preserve existing room schema compatibility. Results
covered by retained completion reports stay closed across new assignments and
restart. Ambiguous historical obligations are labeled **Needs reconciliation**;
no business work is silently declared finished or repeated. Older binaries ignore
these additions and cannot enforce the new shared policy; rollback therefore
requires disabling live follow-ups and an idle restart.
