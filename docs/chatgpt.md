# Participate from the ChatGPT website

ChatGPT can join a MoHuddle room as `chatgpt`, read shared conversation, publish selected results, request feedback or moderated rounds, and assign authorized work. You can continue a private conversation in the same ChatGPT chat. Only text submitted through publishing, work, or round tools enters the shared room; MoHuddle does not receive your ChatGPT conversation history.

The connection uses OpenAI's **Secure MCP Tunnel** and a local, room-specific grant. There is no public MoHuddle URL, HTTP MCP listener, inbound firewall rule, or port forwarding to configure. The tunnel client initiates outbound HTTPS connections to OpenAI and launches MoHuddle's MCP process over standard input/output. See the [official Secure MCP Tunnel guide](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels).

## Requirements

- A current MoHuddle build on Linux/WSL or macOS with the private local API enabled (the default). The Windows native build does not currently provide this Unix-socket transport; use WSL for this integration.
- Access to Secure MCP Tunnel and developer-mode apps in the ChatGPT account/workspace you intend to use. Availability and permissions are controlled by OpenAI and your workspace administrator.
- The official [`tunnel-client`](https://github.com/openai/tunnel-client/releases/latest), installed in the same OS environment and user account as MoHuddle. On WSL, run both in the Linux distribution.
- An OpenAI tunnel runtime API key. It is used by `tunnel-client`, not by MoHuddle or a second background model. Keep it out of source files, command arguments, and chat messages.

## 1. Enable the room connection

Build and install the updated executable, then start or resume the room:

```bash
make install
mohuddle
```

In MoHuddle:

```text
/join @chatgpt
```

MoHuddle manages the private room grant internally. Access lasts eight hours by default. `/chatgpt on 30m` chooses a shorter lifetime; the supported range is one minute to 24 hours.

In a second terminal, verify the connection:

```bash
mohuddle chatgpt doctor
```

This finds the open, authorized room and verifies authentication and the restriction on room controls without joining, reading history, or posting a message. Keep the MoHuddle room open. If multiple authorized rooms are open, select one with `--room ROOM_ID`. For a custom state directory, add `--state-dir DIRECTORY` to both `doctor` and `serve`. Explicit `--connection FILE` remains available for advanced configurations.

## 2. Create and run the private tunnel

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
tunnel-client run --profile mohuddle-chatgpt
```

Keep `tunnel-client run` running in that terminal. The executable can be found with `command -v mohuddle` after installation. The connection file contains only a MoHuddle room grant; the OpenAI runtime key stays with the official tunnel client. No MoHuddle administrator credential is used.

Limit the tunnel's associations and app access to the people who should see this room. Anyone authorized to use this particular tunnel/app can attempt to claim its available ChatGPT seat; the local grant is scoped to the room, not to an individual OpenAI user. Only one ChatGPT conversation can hold that seat at a time.

## 3. Connect it in ChatGPT

Enable developer mode under **Settings → Security and login**, if it is available for your account. Open [ChatGPT Plugins](https://chatgpt.com/plugins), use the plus button to create a developer-mode app, select **Tunnel** under **Connection**, and select or paste your tunnel ID. Use the private tunnel connection rather than a URL. See [OpenAI's connection instructions](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels#connect-from-chatgpt).

Set **Authentication** to **No Authentication** in the ChatGPT app form. This stdio bridge does not implement an OAuth server, so selecting OAuth produces an “MCP server … does not implement OAuth” error. ChatGPT supports this mode in its [developer-mode authentication options](https://developers.openai.com/api/docs/guides/developer-mode). Keep the existing tunnel selected when correcting this setting.

This setting skips an additional OAuth login to MoHuddle. OpenAI still controls access to the private tunnel through its organization/workspace permissions, and MoHuddle validates its local room grant on every room request. The grant is held by the bridge and does not distinguish between OpenAI users who can use that tunnel. Anyone authorized to use this tunnel/app can attempt to claim an available ChatGPT seat and read the shared room. Keep the app private and restrict tunnel access to the intended users; a workspace association alone does not establish that only you have access.

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

## Side conversations and ongoing participation

The live panel starts with automatic follow-ups **paused**. You can read the room while having a private side conversation in ChatGPT, then ask ChatGPT to publish a selected result. Publishing is the explicit sharing boundary, so avoid asking it to post your whole private conversation.

Select **Enable live follow-ups** to let the panel request a ChatGPT turn when new human/peer messages arrive. ChatGPT reads those messages and decides whether a useful contribution is warranted. The panel waits for requested peer replies and queued/running work to finish, ignores ChatGPT's own posts, and limits automatic notifications to eight within 15 minutes, with at least 20 seconds between them.

Use **Pause for side conversation** before privately discussing the result. It prevents further automatic notifications; a turn already requested from ChatGPT may still need to be stopped in the ChatGPT UI. **Ask ChatGPT to review updates** requests one review manually.

ChatGPT controls tool approvals and whether component notifications start a new model turn. A panel cannot guarantee background execution when the chat is closed, suspended, or the host declines a request. If the host does not support follow-ups, use the ChatGPT composer to ask it to read the room. During an active turn, `mohuddle_read` can wait up to 25 seconds for peer replies. This integration does not automate the ChatGPT browser or run a separate API model behind your website conversation.

## Room controls

| In MoHuddle | Effect |
| --- | --- |
| `/join @chatgpt` | Create/rotate an eight-hour private grant. |
| `/chatgpt on 30m` | Create/rotate a grant with a chosen lifetime. |
| `/chatgpt status` | Show connection, expiry, pause, and exchange budget. |
| `/stop` | Pause ChatGPT posting and cancel pending peer replies along with other room work. |
| `/chatgpt resume` | Resume posting and authorize eight more peer exchanges or work requests. |
| `/leave @chatgpt` or `/chatgpt off` | Immediately revoke the grant, delete its private file, and cancel pending peer replies. |
| `/leave @all` | Revoke ChatGPT access and remove local participants. |

Work already accepted by the scheduler follows the normal room lifecycle even if ChatGPT leaves, loses its lease, or its grant is revoked. Use Esc or `/stop` in MoHuddle to cancel running and queued work.

ChatGPT can also call `mohuddle_leave`; this releases its seat and cancels pending replies while leaving the local grant enabled. A participation lease lasts two minutes and is renewed by reads, including panel refreshes. An idle conversation must rejoin after its lease expires. A second conversation cannot take over an active lease.

Grant rotation, room exit, or restart invalidates existing access. The stdio bridge validates the private connection file on each room request, so renewing access to the same room and socket does not require restarting the tunnel. Ask ChatGPT to join again after renewal; previous participation IDs remain invalid. Missing, expired, or unsafe connection files block access immediately. The bridge stays pinned to its selected room and socket. To select a different room, enable it locally and restart the tunnel; automatic discovery then selects the open authorized room, or requires `--room` if there is more than one. `/join @all` only joins configured local providers; granting ChatGPT access is explicit.

## Security boundaries and limits

- A fresh 256-bit grant is stored in a `0600` file beneath a `0700` directory. The host retains its hash in memory. Expiry and revocation are checked for every request, including already-connected clients. Grants are not persisted in room state or returned to ChatGPT.
- The bridge accepts only a private Unix socket and a private regular connection file. It refuses ordinary MoHuddle administrator credentials. Its stdio protocol has bounded frames; it has no network listening option.
- ChatGPT can call only join, read, publish, request_work, request_round, and leave for the granted room. The work endpoint accepts one local participant and task text, using the normal scheduler, workspace write lease, permission ceiling, and approvals. Rounds use that scheduler with a read-only permission ceiling and the native round runner. Generic API history, room controls, approvals, filesystem grants, and command invocation remain denied regardless of assigned scopes.
- Room output exposes shared human/AI text, authors, sequence references, and bounded reply/work status. It excludes tool logs, attachments, internal errors, provider session IDs, socket paths, and credentials. Existing room text is visible after joining, including text humans or peers have already placed in the shared transcript; there is no automatic redaction of secrets someone posts as message text.
- A contribution is AI-authored data. Command-looking text cannot invoke a command. Peer replies use ephemeral read-only turns; their control fields cannot authorize work, change the roster, or update correction records. Provider approval requests during those turns are automatically denied. Read-only peer replies cannot promote themselves into work. An explicit `mohuddle_request_work` call can assign the user's requested task while preserving ChatGPT authorship.
- Publishing and work requests are idempotent when retried with the same operation ID and identical action/content. Limits are 16,000 bytes per request, 20 new posts/work requests per minute, four pending peer replies, four unfinished work requests, and eight peer exchanges/work requests per host authorization. A new local human message or `/chatgpt resume` refreshes the exchange budget. Read pages are bounded to 100 messages, with individual long messages shortened.
- The panel has no external scripts or network destinations. Room text is rendered as text. Its notifications contain fixed instructions and cursors, not copied room content. Follow-ups are opt-in and bounded.
- The OS account running MoHuddle and `tunnel-client` is trusted. Keep its state directory private. The tunnel and room grants do not isolate other processes running as that same OS user.

## Troubleshooting

- **Tunnel unavailable in ChatGPT:** check developer-mode access, the tunnel's ChatGPT workspace association, and Tunnels Read + Use permissions. A Platform organization association alone may be insufficient.
- **“MCP server … does not implement OAuth”:** select **No Authentication** in the ChatGPT app form for this private stdio connection. If that option is selected and the error persists, record the exact error and selected settings before changing the working tunnel configuration.
- **Invalid/expired connection:** enable ChatGPT in the MoHuddle room, then ask ChatGPT to join again. Current builds reload renewed grants for the same room automatically. If an older bridge is still running, restart the tunnel once after upgrading MoHuddle.
- **Another conversation is participating:** leave that conversation's MoHuddle session, close its panel and wait two minutes, or rotate the grant locally.
- **Paused or exchange limit reached:** use `/chatgpt resume` in MoHuddle, then re-enable live follow-ups in the panel if desired.
- **Peer reply pending:** only selected, present local peers are eligible. They may be waiting for provider capacity. Leaving/revoking ChatGPT cancels those requests.
- **Posted but nobody responded:** inspect `action` and `agent_scheduled`. For an answer, use `request_replies`; for a moderated round, use `mohuddle_request_round`; for edits, use `mohuddle_request_work`. Mentions and command text do not dispatch.
- **A message describes several future stages:** only the explicit tool operation runs. Read its completed result and issue a separate tool call for each dependent stage. Do not infer that a published plan is running.
- **Work queued:** read its `work` state; it may be waiting for the workspace writer or provider capacity. Do not resubmit with a new operation ID. Any required approval remains in MoHuddle.
- **A tool is absent after upgrading:** restart the MoHuddle room and tunnel process to load the new binary and schema, then rejoin the resumed room. Update the connection's saved metadata using the controls available in ChatGPT's plugin menu and start a new conversation if necessary; a new chat alone does not rescan the server. Verify seven tools, including `mohuddle_request_round` and `mohuddle_request_work`. Do not substitute command text for a missing tool. Existing grants are process-local.
- **No automatic turn:** the website may require a tool approval or decline component follow-ups. Ask in the ChatGPT composer to read the room and contribute if useful. Keep both the room and tunnel client running.

## Validation

`make check` covers grant isolation, bounded history, retries, cancellation, peer permissions, writable work dispatch, write-lease queuing, approval routing, MCP tool discovery and round trips, and a subprocess running the real stdio command. The embedded panel tests use Node and also run explicitly in CI. Run them separately with `node --test internal/chatgpt/panel_test.mjs`.

Connecting an actual ChatGPT account requires its tunnel, runtime key, and account-side app setup. Local tests do not prove account entitlement or website follow-up behavior.
