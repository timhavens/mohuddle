# MoHuddle

MoHuddle is a local terminal room where you and multiple coding agents can discuss and work on the same project. It supports OpenAI Codex, Anthropic Claude Code, Google Antigravity (`agy`), and GitHub Copilot. MoHuddle launches the providers' command-line tools already installed on your computer, streams their public responses and tool activity into one TUI, and keeps you in the conversation.

```text
You ─┬─ Codex CLI   ── OpenAI
     ├─ Claude CLI  ── Anthropic
     ├─ AGY CLI     ── Google
     └─ Copilot CLI ── GitHub
```

MoHuddle does not call provider model APIs directly and does not store provider credentials or API keys. Each CLI or its official SDK transport connects with the account and authentication method configured by its user.

> **Experimental federation is available.** Two MoHuddle instances can now be
> paired explicitly using short-lived invitations and pinned TLS identities.
> Start with the [federation quick start](#federation-quick-start), then see the
> [complete v1 protocol and security notes](docs/api-v1.md).

> **Parallel auxiliary workers are available.** Configure stable helper
> identities such as `codex-1` and `claude-1` with `/workers`, then hand them
> independent tasks with `/delegate`. See
> [Auxiliary workers and delegation](#auxiliary-workers-and-delegation).

## Features

- One terminal conversation shared by you and any combination of Codex, Claude, AGY, and Copilot.
- New rooms start with Codex and Claude present. `/join` and `/leave` change the roster and save it with the room.
- Ordinary messages use a quiet, sequential floor among the room's active core peers, managed by a selected moderator.
- Core peers privately assess task fit; MoHuddle selects the lead, then guarantees read-only review by the other active cores before the moderator closes.
- Codex and Claude are the preferred cores by default. AGY and Copilot are ordered fallbacks and can be promoted automatically or manually without changing their identity, permissions, model, or saved session.
- Direct `@agent` messages invoke exactly that participant, without automatic review calls.
- Configurable auxiliary identities (`codex-1`, `claude-1`, and so on) keep independent sessions and can execute explicitly delegated subtasks concurrently.
- A persistent, host-derived workboard shows each AI's assignment, role, phase, elapsed time, last activity, stalled state, and queued human input without adding status chatter to the transcript. `/progress compact|detailed|off` controls it.
- An optional persistent `/sound on` setting rings the terminal bell whenever a visible AI turn finishes.
- Independent, persistent model, reasoning-effort, and permission settings for every provider.
- Native provider session IDs and transcript cursors are saved and resumed. A returning agent catches up on messages sent while it was away.
- Public messages and concise tool summaries are stored in an append-only room transcript.
- A versioned command-and-event API serves authenticated local clients over an OS-protected Unix socket and explicitly paired instances over pinned TLS.
- Filesystem grants and approval prompts keep additional directory access explicit.
- `/ask [@agent ...] MESSAGE` retains explicit one-shot parallel participation when it is useful.
- `/round [@agent ...] MESSAGE` gathers selected voices sequentially and ends with read-only moderator synthesis.
- Moderated rounds are structurally bounded: each non-moderator may be invited at most once before the floor closes or returns to you.
- Optional per-agent text-to-speech speaks completed conversational responses through one interruption-safe audio queue.

## Supported environment

MoHuddle currently targets Linux, including Linux distributions running under WSL 2. Release builds are produced for Linux `amd64` and `arm64`.

Runtime requirements:

- A terminal with color and alternate-screen support.
- The [Codex CLI](https://learn.chatgpt.com/docs/codex/cli), installed and authenticated.
- [Claude Code](https://code.claude.com/docs/en/getting-started), installed and authenticated.
- [Bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) for Claude's Linux sandbox.
- Optionally, [Google Antigravity CLI](https://www.agy.dev/docs/cli/getting-started/) and/or [GitHub Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/install-copilot-cli), installed and authenticated before joining them to a room.
- Optionally, `edge-tts` and `mpv` for spoken AI responses.
- Internet access and provider entitlements for every agent you use.

Go 1.25.1 or newer is required only when building or installing MoHuddle from source.

### Install the AI command-line tools

Follow the providers' current installation instructions. At the time this README was written, Codex documents this installer for Linux:

```bash
curl -fsSL https://chatgpt.com/codex/install.sh | sh
codex
```

The first `codex` run offers the available sign-in methods. Claude Code recommends its native installer for Linux and WSL; launch it once afterward and follow the authentication prompts:

```bash
curl -fsSL https://claude.ai/install.sh | bash
claude
```

Install Bubblewrap on Ubuntu or Debian:

```bash
sudo apt-get update
sudo apt-get install bubblewrap
```

AGY and Copilot are optional. Install either one only if you want that participant available in `/agents`:

```bash
curl -fsSL https://antigravity.google/cli/install.sh | bash
agy

npm install -g @github/copilot
copilot login --device-code
```

An interactive `agy` run performs Antigravity authentication. `copilot login --device-code` is convenient in WSL or another headless terminal; Copilot also supports its web login and documented token environment variables. Each provider has its own account, subscription, quota, and model availability rules.

Confirm the complete runtime before starting MoHuddle:

```bash
codex --version
codex login status
claude --version
claude auth status
bwrap --version
agy --version
agy models
copilot --version
```

The last three checks are required only when using those optional providers. Each coworker should authenticate with their own provider accounts. Do not copy or share CLI credential files.

### Install optional speech support

On Debian under WSL, install the player and Edge TTS client with Python 3:

```bash
sudo apt install -y python3-pip mpv
python3 -m pip install --user edge-tts
```

Do not use bare `pip` on a system where it may resolve to Python 2. MoHuddle looks for `edge-playback` and `edge-tts` on `PATH`, then in `$HOME/.local/bin`. Confirm audio reaches the Windows speakers with:

```bash
~/.local/bin/edge-playback \
  --voice en-US-AndrewMultilingualNeural \
  --text "Hello from MoHuddle. This should sound considerably better."
```

Edge TTS is an online service, so speech needs Internet access. It does not require an additional AI API key.

## Install MoHuddle

### GitHub release

Download the archive for your processor from the [latest GitHub release](https://github.com/timhavens/mohuddle/releases/latest), verify it against `checksums.txt`, and install the executable:

```bash
tar -xzf mohuddle_VERSION_linux_amd64.tar.gz
install -m 0755 mohuddle "$HOME/.local/bin/mohuddle"
```

Use `linux_arm64` instead of `linux_amd64` on an ARM64 machine. Ensure `$HOME/.local/bin` is on `PATH`.

### Go install

After the first version is tagged on GitHub:

```bash
go install github.com/timhavens/mohuddle/cmd/mohuddle@latest
```

The executable is installed in `$(go env GOPATH)/bin` unless `GOBIN` is configured.

### Build from source

```bash
git clone https://github.com/timhavens/mohuddle.git
cd mohuddle
make check
make install
```

`make install` installs to `$HOME/.local/bin` by default. Override the prefix when needed:

```bash
make install PREFIX=/usr/local
```

To build without installing:

```bash
make build
./bin/mohuddle --version
```

## Quick start

Start MoHuddle from the project directory the agents should share:

```bash
cd /path/to/your/project
mohuddle
```

The launch directory becomes the room's initial read/write workspace. MoHuddle automatically resumes the most recently updated room for that exact workspace. Create a separate room with:

```bash
mohuddle --new
```

Examples:

```text
Review this repository together and identify the riskiest unfinished work.
@codex implement the parser we just discussed.
@claude review Codex's changes and run the relevant tests.
/join @agy
@agy give us an independent review of the result.
/join @copilot
@copilot check for GitHub-specific integration issues.
/round @claude @agy compare your conclusions without changing files.
```

## Conversation behavior

An ordinary message first obtains short, private task-fit bids from every active core peer. These transcript-only bids use disposable provider sessions with no workspace roots or tools, are not written to the room transcript, and cannot change saved provider sessions or cursors. A unique plurality selects the lead; a tie or invalid result falls back to the room moderator—Codex by default. The entire bid phase has a two-second deadline; if it expires, MoHuddle cancels the outstanding bids and immediately assigns the configured moderator.

The moderated core workflow uses one public floor at a time. The selected lead answers or performs authorized work, every other non-moderator core reviews it read-only, and the moderator reviews last. Explicit auxiliary delegations may run concurrently outside that floor; their results are recorded before moderator synthesis. If the moderator led, it gets a separate read-only closing turn after all peer reviews. Marker-only reviews remain publicly silent. One, two, or more cores therefore receive the same scheduling contract regardless of provider.

During its closing turn, the moderator may invite any remaining optional peer for a materially distinct perspective. Each may speak at most once and the floor then returns to the moderator. A neutral request for another response without a named participant automatically advances to the next eligible peer. Only an explicit material disagreement creates the conflict dialog; waiting, malformed routing, cancellation, or provider failure does not fabricate a conflict.

A targeted message such as `@codex implement this function` invokes only Codex with its configured permission profile. Common punctuation immediately after the name is accepted, so `@claude?` and `@claude, please review this` are also direct turns. `@agy` and `@copilot` bypass moderation. At their built-in read-only default those turns remain isolated and tool-free; setting either participant to `workspace` or `full` makes direct turns use its coding tools and saved native session. There is no automatic peer-review loop after a direct message.

Use `/moderator` to show the moderator, `/moderator @agent` to select any active core peer explicitly, or `/moderator auto` to return to automatic selection. If an explicitly preferred moderator becomes unavailable, its promoted replacement moderates when possible; otherwise the first active core takes over. Explicit preference is retained for safe restoration. Humans are never moderated.

Core scheduling is independent of permissions. `/core` shows the effective policy; `/core preferred` and `/core fallbacks` configure the room, with `default` after the subcommand to save personal defaults. Automatic failover is the built-in mode, and only present, installed, available fallbacks are eligible. `prompt` reports an open slot with the manual `/core replace` action; `off` warns but never fills it automatically. Restoration can be `auto`, `prompt`, or `manual` and occurs only at an idle workflow boundary. `/core unavailable` records a confirmed cooldown with an optional retry time; strict provider session/quota signals are also recorded automatically. An ambiguous provider reset time produces an explicit RFC3339 confirmation command instead of changing availability. Ordinary errors and cancellation never change the roster.

You can keep typing while agents work. Every ordinary message is saved immediately and, while work is active, queued durably for the next safe workflow boundary. Consecutive queued messages with the same target are handled together. They are excluded from the running agents' prompts and cannot be skipped by a saved transcript cursor. The queue survives a restart and is labeled both in the transcript and workboard.

Press `Shift+Tab` to switch the room composer between Default and Plan modes.
Plan mode is shown as `PLAN · READ-ONLY`, persists with the room, and affects
future submissions without interrupting active work. Every accepted human
message records its mode, so queued plan messages remain plan-only after the
composer returns to Default mode or MoHuddle restarts. The bid-selected lead
grounds the request and produces one decision-complete terminal
`<proposed_plan>` block. Other active cores review it read-only and concurrently;
silent reviews add no moderator-closing turn, while a material concern returns
to the lead for one final synthesis. Direct `@agent` Plan turns use that agent as
the plan owner without adding a peer-review loop.

The host forces every Plan participant to read-only permissions, removes write
roots and general provider network access, rejects access expansion and
AI-requested roster changes, and instructs the room to plan rather than
implement. When the separate host-mediated research setting is enabled, Plan
and Default turns may still use its narrow public search/page-fetch boundary.
A valid final
plan is stored independently of the transcript window and replaces the composer
with Codex-style choices:

```text
Implement the plan?

[Yes, implement this plan]
[No, stay in Plan mode]
```

Yes is an explicit trusted-local action. It consumes the proposal once, switches
to Default mode, clears native planning sessions, and starts a fresh workflow
with the exact integrity-checked plan injected by the host. No clears the
decision prompt but keeps Plan mode, provider sessions, and transcript context
for further discussion or revision. Pending decisions survive restart. Remote
participate devices can observe the pending proposal but cannot approve it.
`/plan on|off|status` provides the mode control without the keyboard shortcut.

### Host-mediated web research

Public web research is off by default and independent of Default/Plan mode:

```text
/search status
/search on
/search off
```

When enabled, any participant can ask the host for a bounded public search or
for the text of an explicit public HTTPS page. The provider process does not
receive arbitrary network access. The broker uses unauthenticated GET requests
without cookies, credentials, uploads, request bodies, environment proxies, or
nonstandard ports. It revalidates every redirect and DNS result, rejects
localhost, LAN, link-local, metadata, documentation, reserved, multicast, and
other special-purpose addresses, pins each connection to a validated address,
requires TLS, limits redirects/time/response size/content types, and returns
bounded untrusted text with source URLs. Search queries are sent to Brave
Search; opened pages are sent to their named origins. Enable the feature only
when that public egress is appropriate.

Research requests and outcomes appear as ordinary tool activity. A separate
`research_audit.jsonl` stores timestamps, participant/room identity, operation,
input hashes, destination hosts, and sanitized outcomes—not raw queries, full URLs,
credentials, cookies, or page content. The setting never changes an agent's
saved permission profile and never enables shell networking. Full access, when
explicitly granted by the human, remains a separate unrestricted profile.
The broker records the attempt before any egress and fails closed if that audit
record cannot be written.

The provider-boundary audit keeps Codex network-disabled in read-only and
workspace sandboxes, gives Claude an empty sandbox domain allowlist, blocks
Copilot URL-bearing shell requests and common networking commands, and leaves
AGY in its native sandbox for read-only and workspace profiles. The broker is
the uniform research path; enabling it does not loosen any of those adapter
settings. AGY's native sandbox remains provider-owned, so MoHuddle does not
claim method-level enforcement inside that process.

Use `/steer MESSAGE` (or `Ctrl+Enter`) when new direction really should cancel and replace active work. Non-empty public text that was streaming when explicit steering occurs is stored with an `interrupted` label and does not advance that provider's saved cursor. `/stop` cancels every active agent and clears queued input. `/ask`, `/round`, and `/continue` refuse to supersede active work; wait for the boundary, or use `/steer` deliberately.

For a discussion that should explicitly hear from the room, use `/round MESSAGE` for all present agents or select participants such as `/round @claude @agy MESSAGE`. Requested participants speak sequentially, all turns are read-only, individual failures do not prevent later speakers, and the moderator synthesizes last. For independent parallel answers with no synthesis, use `/ask MESSAGE` (or `/once`) or a selected subset such as `/ask @codex @agy MESSAGE`. These discussion workflows never use a saved workspace/full override. Optional read-only peers remain isolated and tool-free; active core peers retain their captured core-session context under read-only enforcement.

Provider calls use one execution lane per agent. Codex, Claude, AGY, and Copilot can therefore overlap with one another, while MoHuddle never starts two simultaneous calls against the same provider session.

Transcript context is always bounded before a provider call: ordinary turns
receive at most the newest 256 records and 256 KiB, while auxiliary-worker
turns receive at most 128 records and 64 KiB. A single oversized record is
truncated safely. This keeps cold or long-absent participants below provider
request limits while preserving the newest room context.

## Auxiliary workers and delegation

MoHuddle can start additional, stable identities backed by the same installed
providers. Each helper has its own provider session, transcript cursor,
activity row, model settings, and permissions. Helpers are not core peers and
cannot moderate the room. Their built-in permission profile is `read-only`;
raise a specific helper's permissions explicitly only when its task genuinely
requires writing. Parallel writers can conflict even when they use different
provider sessions.

Show the current topology and configure it with `/workers`:

```text
/workers
/workers @codex 2 @claude 1
/workers @all 1
/workers @agy 0
/workers off
```

Counts are personal settings, not room-local settings. A topology change is
validated atomically, saved, and reloads the current room so provider clients
are created or retired safely. The limit is three helpers per provider and
eight helpers total. Configured helpers appear as `codex-1`, `codex-2`,
`claude-1`, and so on. Their room membership and saved sessions survive normal
room restarts; `/join @codex-1` and `/leave @codex-1` control whether a
configured helper is currently participating. Worker counts cannot change
while agent work is active.

Delegate an independent task without cancelling the main moderated workflow:

```text
/delegate @codex-1 inspect the parser and report concrete edge cases
/delegate @claude-1 review the persistence migration and run focused tests
```

Separate `/delegate` commands may run different helpers concurrently. The same
helper still has one execution lane, so it cannot accept overlapping tasks.
Delegated turns are always host-enforced read-only even if that identity has
broader settings for direct turns. Use an explicitly targeted normal message
only when a helper genuinely needs its configured write permission; MoHuddle
does not automatically coordinate concurrent writers.
Human-delegated responses are public transcript messages but do not force a new
core review by themselves. The moderator can request a bounded,
host-validated batch of helper tasks; MoHuddle waits for that batch and returns
the results to the moderator for synthesis. It may also suggest configured
helpers joining or leaving. The host rejects unconfigured targets, duplicate
tasks, self/core membership changes, and requests beyond the fan-out limit.
Ordinary new human messages queue without cancelling active work, while issuing
another `/delegate` may still start a distinct helper concurrently. `/steer`
is the explicit replacement path. All identities backed by one provider still share that provider
account's quota and rate limits, so more workers increase parallelism rather
than quota.

Use `/agents` to see which CLIs MoHuddle found and which agents are present. `/leave @agent` removes an idle agent from future rounds; `/join @agent` returns it. Human roster changes are accepted only when no round is active. They are written to `room.json`, do not delete the provider session or cursor, and are restored after restarting MoHuddle. On its next turn, a returning agent receives the room messages added since its last completed response. A moderator may suggest joining or leaving only configured auxiliary workers during its safe closing turn; the host validates and applies that request atomically. An AI can never add an unconfigured identity, change a core participant's membership, or remove a worker that is still active.

Humans can also schedule a future roster change for any configured participant:

```text
/roster show
/roster schedule join @claude at 2026-08-24T09:00:00-04:00 quota reset
/roster schedule join @codex-1 retry
/roster schedule leave @copilot for 30m maintenance
/roster cancel ACTION_ID
```

`retry` requires a confirmed future retry time in that participant's availability
state. A maximum of 32 actions may be pending, and only one may be pending per
participant. The host persists pending, executed, cancelled, and failed records,
then executes due changes only while the workflow is idle. A scheduled join waits
through any current cooldown; restart does not lose it. Roster changes preserve
the participant's provider session and transcript cursor. Only the human TUI or
an authenticated local API client with `administer` scope can schedule or cancel
these actions—AI prose and private control markers cannot.

The agents receive the room transcript, including stored tool summaries and interrupted drafts. Every turn also receives an explicit host-assigned participant identity that transcript content cannot override. Their hidden reasoning is neither displayed nor copied between providers.

Routing, task-fit bids, and sufficient moderator closings stay private. Marker-only completions are not written to the public transcript, and agents are instructed not to post filler such as “no disagreement,” “nothing to add,” or “standing by.”

AI-to-AI correction statistics use optional sequence references in that same private control marker. `corrects` points to the earlier public AI message being corrected; `accepts` and `disputes` point to the correcting response; `retracts` lets the proposer withdraw its own correction. MoHuddle derives identities from the referenced messages, limits references to the transcript that participant actually received, and accepts lifecycle changes only from the relevant target or proposer. It never infers corrections from prose. Host validation rejects user corrections, self-corrections, marker-only claims, unauthorized actions, and duplicate resolutions; the protocol instructs agents not to declare additions, stylistic suggestions, or ordinary disagreements as corrections.

Corrections begin pending. Target acceptance and proposer retraction are terminal; a target dispute remains pending unless the target later accepts or the proposer retracts. Every validated lifecycle event is stored immutably beside its public transcript message and replayed in message-sequence order, making concurrent outcomes deterministic and restart-safe. `/status` reports offered, accepted, retracted, and pending totals plus accepted corrections received by each AI. These are auditable event counts, not reliability or quality scores.

The compact workboard remains visible even before response text arrives:

```text
⠹ CODEX   lead · testing  12s  · implement queued input
○ CLAUDE  idle
○ AGY     away
○ COPILOT idle
↳ QUEUED 2 human message(s) · next safe boundary · /steer applies immediately
```

The host derives assignments and roles from workflow state, and phases from safe tool/status events (`reading`, `planning`, `editing`, `testing`, `waiting`, and `blocked`). A busy row with no meaningful activity for 60 seconds shows `stalled?`. `/progress compact` is the default, `/progress detailed` adds safe current activity summaries, and `/progress off` hides the workboard. This personal setting survives room changes and restarts.

Public response text still streams into the conversation as it arrives. `/details` remains separate: it controls historical tool messages in the transcript, while `/progress` controls only the in-place workboard.

MoHuddle can ring the terminal bell whenever a visible AI turn finishes. Use `/sound on` to enable it, `/sound off` to disable it, or `/sound` to toggle it. The personal setting survives room changes and restarts. It uses the terminal's standard `BEL` notification, so Debian and WSL terminal emulators need their audible bell enabled; some terminals may use a visual notification or ignore it according to their own preferences. No speech provider or desktop audio service is required.

The composer keeps up to 200 submitted entries per room and restores them after a restart. Up and Down recall history for a single-line draft; Ctrl+P and Ctrl+N always move through history. MoHuddle preserves the unfinished draft, compact pasted blocks, and attached images while history is being browsed. Its compact, unnumbered input expands for multiline drafts. The context footer shows every active core peer's effective model, effort, permission profile, and workspace; color highlights the peers the current input targets.

Multiline or large pasted text is kept in full but displayed as a compact `Pasted Content` item until sent. Ctrl+V also checks the Windows image clipboard under WSL and displays a compact image item. Codex receives images through its native local-image input, Copilot through an SDK attachment, and Claude receives a private saved path it can read. AGY currently cannot inspect images; MoHuddle shows a warning and continues with the other selected participants. Room attachments and composer history are stored privately alongside room state rather than in the workspace.

PageUp/PageDown and Ctrl+Up/Ctrl+Down scroll the conversation without moving focus from the composer. Ctrl+Home/Ctrl+End jump to the beginning or end, and the mouse wheel scrolls the same viewport. New agent output no longer forces the screen to the bottom while you are reading earlier content; the footer reports how many new messages are waiting.

If MoHuddle exits while an agent is working, that active provider turn is cancelled. Completed messages and non-empty interrupted public drafts remain saved; queued human input resumes automatically at startup, while an interrupted in-flight turn itself does not. Use `/continue` or send the active request again when needed.

## Keyboard controls

```text
Enter       send now when idle; otherwise save and queue the message
Shift+Tab   toggle execute and host-enforced plan mode for future messages
Ctrl+Enter  explicitly steer: cancel and replace active work
Alt+Enter   insert a newline
Up/Down     recall history for single-line input
Ctrl+P/N    previous/next history entry
PageUp/Down scroll the conversation by a page
Ctrl+Up/Down
            scroll the conversation by one line
Ctrl+Home   jump to the top of the conversation
Ctrl+End    jump to the bottom and resume auto-follow
Ctrl+V      paste text or attach a clipboard image
Tab         complete the selected slash-command suggestion
Alt+M       toggle mouse scrolling or normal terminal text selection
Alt+V       toggle speech on or off
Esc         dismiss suggestions; decline a plan decision; otherwise stop active and queued work
Ctrl+C      exit cleanly
```

When no suggestion or decision overlay is open, `Esc` cancels all active work
and clears queued input, matching `/stop`. Use `Ctrl+Enter`/`/steer` to replace
active work deliberately instead.

Mouse scrolling is enabled by default. Press `Alt+M` to release mouse capture for normal drag selection, then press it again to restore mouse scrolling. In Windows Terminal, Shift+drag can also select text while mouse capture remains enabled.

When an approval dialog is visible, use the keys shown in the dialog instead of typing a chat message.

## Room commands

```text
/agents                    list supported agents as present, away, or unavailable
/workers [show|off|@all N|@provider N ...]
                           show or configure auxiliary AI identities
/delegate @worker TASK     run an independent helper subtask without cancelling the main workflow
/plan [on|off|status]      toggle, set, or show host-enforced Plan mode
/search [on|off|status]    set or show host-mediated public web research
/steer MESSAGE             cancel and replace active work with explicit new direction
/progress [compact|detailed|off]
                           show, expand, or hide the in-place workboard
/roster [show]             show scheduled roster-action history
/roster schedule join|leave @agent for DURATION [REASON]
/roster schedule join|leave @agent at RFC3339 [REASON]
/roster schedule join @agent retry [REASON]
/roster cancel ACTION_ID   cancel a pending scheduled roster action
/remote [devices]          show the configured phone gateway and paired devices
/remote pair observe|participate|admin DEVICE_NAME
                           create a 15-minute single-use device invitation
/remote scope DEVICE_ID observe|participate|admin
                           change scope and close credentials issued before it
/remote revoke DEVICE_ID   revoke a device and close its active sessions
/remote audit              show recent credential-free remote activity
/core [show]               show preferred, active, fallback, and unavailable peers
/core preferred [default] @agent [@agent ...]
/core fallbacks [default] @agent [@agent ...]|none
/core failover [default] auto|prompt|off
/core restoration [default] auto|prompt|manual
/core inherit              remove the room core-policy override
/core promote @agent       add a temporary core peer
/core replace @preferred @fallback
                           temporarily fill one preferred-core slot
/core demote @agent        remove a temporary promotion
/core restore [@agent|all] restore available preferred core peer(s)
/core unavailable @agent [for DURATION|until RFC3339] [REASON]
/core available @agent     clear confirmed availability state
/moderator [@agent|auto]
                           show or change the room moderator
/ask [@agent ...] MESSAGE  one concurrent response per selected/present agent
/round [@agent ...] MESSAGE
                           sequential read-only discussion with moderator synthesis
/join @agent|@all          add installed agent(s) to future rounds
/leave @agent|@all         remove installed agent(s) from future rounds
/continue                  start another bounded moderated round
/stop                      interrupt all active work and clear queued input
/details [on|off]          toggle or set behind-the-scenes tool/activity detail
/sound [on|off]            toggle or set the AI-finished terminal bell
/speak [on|off|all|@agent|stop|skip]
                           show or control spoken responses
/voice @agent [VOICE|off]  show, set, or clear an agent's voice
/voices [FILTER]           list available Edge voices
/status                    show room, core, correction, provider, and session status
/settings                  show effective settings, personal defaults, and command examples
/models @agent             list that provider's selectable models and effort levels
/model [default] @agent|@all MODEL
/effort [default] @agent|@all LEVEL
/permissions [default] @agent|@all PROFILE
/inherit @agent|@all
                           remove a room override and inherit personal defaults
/access                    show filesystem grants for the room
/revoke [@agent|@all] PATH
                           revoke a matching non-workspace grant
/rooms                     list saved rooms
/new                       switch to a new room
/resume ROOM_ID            switch to an existing room
/help                      show command help
/quit                      exit cleanly
```

## Command-line options

```text
--workspace PATH           use PATH as the initial workspace instead of .
--room ID                  resume a specific saved room
--new                      create a room instead of resuming the latest one
--max-waves N              deprecated compatibility option
--max-turns N              deprecated compatibility alias
--codex-binary PATH        use a non-default Codex executable
--claude-binary PATH       use a non-default Claude executable
--agy-binary PATH          use a non-default AGY executable
--copilot-binary PATH      use a non-default Copilot executable
--codex-model MODEL        override the Codex CLI model
--claude-model MODEL       override the Claude CLI model
--agy-model MODEL          override the AGY model
--copilot-model MODEL      override the Copilot model
--codex-effort LEVEL       override Codex reasoning effort
--claude-effort LEVEL      override Claude reasoning effort
--agy-effort LEVEL         override AGY reasoning effort
--copilot-effort LEVEL     override Copilot reasoning effort
--codex-permissions NAME   use read-only, workspace, or full for Codex
--claude-permissions NAME  use read-only, workspace, or full for Claude
--agy-permissions NAME     use read-only, workspace, or full for AGY
--copilot-permissions NAME use read-only, workspace, or full for Copilot
--state-dir PATH           override local room storage
--config PATH              override the personal settings file
--api-socket PATH          override the room-specific local API socket
--no-api                   disable the local API
--federation-listen ADDR   explicitly enable the TLS federation listener
--remote-listen ADDR       explicitly enable the phone web gateway
--remote-origin ORIGIN     exact allowed browser origin for that gateway
--remote-tls-cert PATH     TLS certificate for the phone web gateway
--remote-tls-key PATH      TLS private key for the phone web gateway
--version                  print the MoHuddle version
```

`--room` and `--new` cannot be used together. The old wave/turn cap options remain parseable for compatibility but do not control the structurally bounded moderated workflow.

## Models, effort, and permissions

Models, effort, and permission profiles are configured independently for every provider. Effective settings use this precedence:

1. Command-line override for the current MoHuddle process.
2. Saved room override.
3. Personal default.
4. MoHuddle's built-in default.

Use `/models @agent` for the provider's current catalog. Codex and Copilot load account-aware catalogs from their runtimes, AGY runs `agy models`, and Claude shows its stable aliases; full provider model IDs are also accepted. Examples:

```text
/model @codex gpt-5.6-sol
/effort @codex high
/model @claude opus
/effort @claude xhigh
/model @agy gemini-3.7-flash-high
/effort @agy high
/model @copilot auto
/effort @copilot medium
/permissions @all workspace
```

Those commands change only the current room. Insert `default` after the command name to save a personal default used by rooms without overrides:

```text
/model default @claude sonnet
/permissions default @codex full
```

Use `default` as a model value or `auto` as an effort value to clear that provider override. Model or effort changes may reset the affected native provider session; MoHuddle replays the saved room transcript so the public conversation continues.

Effort values are validated per provider: AGY accepts `auto`, `low`, `medium`, or `high`; Claude accepts `auto`, `low` through `max` except `minimal`; Copilot accepts `auto`, `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max`; Codex accepts the levels its model advertises.

Permission profiles are:

- `read-only`: the selected agents can inspect granted roots but cannot make changes.
- `workspace`: the selected agents can edit and run commands without routine approvals inside granted roots; network access is blocked where the provider transport can enforce it. This is the built-in default for Codex and Claude.
- `full`: provider approvals and MoHuddle sandboxes are disabled, giving that agent unrestricted host filesystem and network access.

AGY and Copilot default to `read-only`. While optional, their direct and invited turns are isolated and transcript-only; when promoted to active core, the same profile permits persistent read-only inspection and normal core participation without mutation. Set either to `workspace` or `full` when direct or lead turns should perform coding work. Demotion restores the optional isolated behavior without deleting its native session. Core review, moderator closing, `/round`, and `/ask` turns remain read-only without changing any saved profile.

The first `full` selection requires typing `FULL ACCESS` exactly. The acknowledgement is saved in the personal settings file, so a saved full-access default starts without repeated confirmations. MoHuddle displays a red `FULL ACCESS` badge whenever a present agent uses this profile. Only enable it on a machine and in repositories you trust: a model mistake or prompt injection can read, change, transmit, or delete data before a conversational disagreement is detected.

Personal settings are stored at `$XDG_CONFIG_HOME/mohuddle/config.json`, falling back to `$HOME/.config/mohuddle/config.json`, with file mode `0600`.

## Local API and federation

### Local Unix endpoint

On Unix platforms, each running room also exposes the versioned `mohuddle.v1`
JSON-lines protocol through a mode-`0600` socket in MoHuddle's private state
directory. The first launch creates a mode-`0600` local credential file; all
connections authenticate, receive an immutable namespaced identity and scopes,
and are recorded in an append-only connection audit. Room/history views omit
workspace paths, grants, native sessions, settings, and attachment host paths.

The v1 API supports room binding, sanitized snapshots and history, status,
ordinary/one-shot/round messages, a narrow command allowlist, agent activity,
and streaming results. Routed mutations carry global message IDs, authenticated
origin identities, hop metadata, restart-safe deduplication, and loop prevention.
Peer and hosted-bridge credential kinds are restricted to read-only `/ask`
participation and cannot inherit local tool or filesystem permissions.

### Secure phone access

Phone access is disabled by default. To place the embedded, mobile-friendly PWA
behind an encrypted authenticated tunnel or reverse proxy, bind only on
loopback and configure the exact public HTTPS origin:

```bash
mohuddle \
  --remote-listen 127.0.0.1:8787 \
  --remote-origin https://phone.example
```

For a non-loopback listener, MoHuddle requires TLS and an exact browser origin:

```bash
mohuddle \
  --remote-listen 0.0.0.0:8443 \
  --remote-origin https://phone.example \
  --remote-tls-cert /secure/path/fullchain.pem \
  --remote-tls-key /secure/path/key.pem
```

MoHuddle does not create a tunnel, configure DNS, or open a firewall. Keep the
listener on a private network or behind an authenticated tunnel/reverse proxy
whose public origin exactly matches `--remote-origin`.
The proxy must preserve that public `Host` header. Configuring an HTTPS public
origin also makes the browser session cookie `Secure` even when the private
loopback hop is cleartext. For local-only testing, omit `--remote-origin` and
open the loopback HTTP address directly.

Create the device invitation only from the trusted local TUI:

```text
/remote pair observe Tim's phone
/remote pair participate Tim's phone
/remote pair admin Tim's phone
```

The command displays a 15-minute, single-use code and fragment URL. The browser
creates a P-256 key locally; the private key is non-extractable and stored in
IndexedDB. MoHuddle persists only the public device grant and a hash of the
unused invitation. Ordinary browser sessions use signed short-lived challenges,
an HttpOnly SameSite cookie, and CSRF protection, and expire independently from
the device grant.

`observe` devices can read the sanitized room. `participate` devices may also
send only isolated read-only `ask` turns. Both have a fixed `read-only`
execution ceiling regardless of the agents' saved workspace/full settings.
The phone composer labels that ceiling explicitly; imperative wording never
elevates a phone turn into workspace execution. A participate device also has a
confirmed **Stop all work** control, and exact `/stop` composer input invokes
the same narrow operation instead of becoming chat text. Stop cancels active
agents and clears queued input, but grants no other room-control or admin
authority. Observe devices cannot use it. A trusted-local `/remote pair admin`
or `/remote scope ... admin` elevation adds only confirmed workflow controls:
approve or decline the exact persisted pending plan ID, switch Plan/Default
mode, continue, and stop. It cannot invoke roster/general commands, and its AI
messages remain read-only. Scope changes invalidate all prior device sessions.

The PWA reconnects with a process-boot event cursor and durable transcript
sequence. Event replay and subscriber queues are bounded; restart, expiry, or
either upstream/downstream overflow produces a typed gap and lossless transcript
recovery. Initial synchronization is one bounded, stable-through history page;
the PWA fetches remaining pages itself and advances its event cursor only after
each replay frame arrives. The service
worker caches only the application shell—never API responses, transcripts,
cookies, pairing material, or drafts.

Inspect and revoke access locally:

```text
/remote devices
/remote scope DEVICE_ID observe|participate|admin
/remote audit
/remote revoke DEVICE_ID
```

Revocation persists, invalidates sessions, and closes active WebSockets.
Device/session identities, scopes, read-only ceiling, denied authentication,
requests, and revocation are written without secrets to `api_audit.jsonl`.

### Federation quick start

Federation is disabled by default. Pairing uses a short-lived, single-use
invitation plus pinned TLS instance certificates; there is no discovery or
automatic LAN joining. On the host, create an invitation with the exact address
that the other instance can reach, then explicitly start the listener:

```bash
mohuddle pair invite --address HOST:7443 > pair.invite
mohuddle --federation-listen 0.0.0.0:7443
```

On the other instance, accept the invitation through stdin so its secret does
not enter shell history:

```bash
mohuddle pair accept < pair.invite
mohuddle pair list
mohuddle pair check --peer HOST_INSTANCE_ID --room REMOTE_ROOM_ID
```

`mohuddle pair revoke INSTANCE_ID` removes both inbound and outbound grants and
terminates active event streams. Pairing is directional; repeat the exchange in
the other direction if both instances must initiate connections. Opening the
listener beyond localhost is an explicit network exposure, so firewall it to
the intended peer or use a private network/tunnel.

This milestone pairs the instances' command-and-event APIs; it does not
automatically mirror two TUI rooms or add a remote AI to the local roster.
`pair check` proves the remote identity, room binding, and sanitized status path.
Protocol clients can also read history, submit isolated read-only `ask` turns,
and stream events. Interactive room mirroring and hosted-service relays remain
future layers built on the same authenticated transport.

See [docs/api-v1.md](docs/api-v1.md) for the wire format, credential locations,
scopes, request types, routing rules, and current limitations. Outbound
hosted-service relays and the Windows named-pipe transport are not implemented
yet; there is no HTTP, WebSocket, unauthenticated, or automatic LAN listener.

## Core peers and failover

The built-in core policy prefers `@codex @claude`, tries `@agy @copilot` as fallbacks, and uses automatic failover and restoration. Only fallbacks already present in the room are promoted; an intentionally away participant is never joined implicitly. Examples:

```text
/core
/core replace @claude @agy
/core restore @claude
/core preferred @codex @agy
/core fallbacks @claude @copilot
/core failover prompt
/core restoration manual
/core preferred default @codex @claude
/core fallbacks default @agy @copilot
/core inherit
```

`/core promote @agent` adds a manual temporary core without replacing a preferred slot. `/core replace` also creates a manual override, which remains pinned until `/core restore`, `/core demote`, or a policy change removes it; automatic restoration applies only to availability- and presence-driven failover. With automatic failover enabled, an automatic replacement cannot be removed while its preferred source remains unavailable; mark the source available, rejoin it, or change failover mode first.

For a known provider cooldown, use `/core unavailable @claude for 2h session limit` or an absolute RFC3339 timestamp. `/core available @claude` clears it. Confirmed Copilot account/session quota and global user rate-limit events, plus Claude-style session-limit errors with an unambiguous reset timezone, are detected automatically. Model- or integration-scoped limits remain ordinary errors so they cannot silently rearrange the room. An otherwise recognizable reset with an uncertain timestamp or timezone emits an actionable `/core unavailable ... until RFC3339` confirmation instead of mutating state. Repeated confirmed failures refresh automatic cooldown state but never overwrite a manual hold. Retry expiry makes the preferred core eligible at the next idle workflow boundary; `/roster schedule join @agent retry` can also return an intentionally away participant then.

Preferred policy, temporary promotions, availability, moderator preference, provider sessions, and permissions are persisted separately. Promotion changes only scheduling: the fallback keeps its own identity, access profile, grants, model, effort, transcript cursor, and native session.

## Spoken responses

Speech starts disabled and no agent has a voice by default. MoHuddle supports the existing Edge provider and a local Kokoro provider. Kokoro is the recommended path because response text remains on the machine after its one-time runtime/model installation.

Install the pinned user-local Kokoro runtime and checksum-verified full-quality model without changing system Python:

```bash
./scripts/install-kokoro-tts.sh
go run ./cmd/tts-smoke --voice am_adam
go run ./cmd/tts-smoke --configure
```

The smoke command speaks through the same persistent-worker and `mpv` path used by MoHuddle. The installer writes under `$XDG_DATA_HOME/mohuddle/tts/kokoro`, falling back to `$HOME/.local/share/mohuddle/tts/kokoro`. It installs `uv` 0.12.5, a private Python 3.12 runtime, the fully version-locked `kokoro-onnx` 0.5.0 dependency set, the full Kokoro v1.0 ONNX model, and the shared voice bank. It does not install system packages. `mpv` must already be available.

The `--configure` command preserves other personal settings, selects Kokoro with the tested four-agent voice preset, and deliberately leaves speech off. Restart MoHuddle and use `/speak all` when ready. To configure manually or customize the preset, edit the personal configuration while MoHuddle is stopped:

```json
{
  "version": 3,
  "speech": {
    "enabled": true,
    "provider": "kokoro",
    "mode": "all",
    "voices": {
      "codex": "am_adam",
      "claude": "af_sarah",
      "agy": "am_michael",
      "copilot": "af_nova"
    },
    "announce_agent": false,
    "max_segment_chars": 240,
    "worker_nice": 5
  }
}
```

The runtime, model, voice-bank, and player paths use the defaults above. Advanced installations can set `python_binary`, `model_path`, `voices_path`, or `player_binary`. A positive `worker_nice` value gives active agents and builds priority over local synthesis under CPU contention; the default is `5` on platforms that support process niceness.

Start MoHuddle and inspect or change the configured voices normally:

```text
/voices
/voice @codex am_adam
/voice @claude af_sarah
/speak all
```

`/speak all` speaks every mapped agent; `/speak @codex` selects only Codex. An agent without a configured voice is skipped silently. `/speak off` immediately stops playback, clears the queue, and disables future speech. `/speak stop` also stops and clears the queue but leaves speech enabled, while `/speak skip` stops only the current completed response and continues with the next queued response. `Alt+V` is the quick on/off toggle. The footer badge shows whether speech is off, active, queued, or unavailable.

MoHuddle speaks only completed conversational AI messages. It does not send streaming tokens, tool activity, interrupted drafts, command output, or status events to TTS. A speech-only copy removes Markdown presentation and URLs; inline code keeps its contents without the backticks, while fenced code, tables, structured data, stack traces, and similar non-natural material are replaced by one combined cue such as “Refer to the code and table on screen.” The original on-screen message is never changed.

Kokoro keeps one model worker warm, synthesizes sentence-sized calls ahead of playback, and feeds raw PCM through a small cancelable Go-side reserve to one `mpv` process. The player remains open for a 30-second idle grace period so nearby responses avoid reopening the audio device, then closes cleanly instead of maintaining indefinite background audio transport. The reserve absorbs slower sentence calls without giving `mpv` seconds of audio that cannot be flushed. Under WSL, the player receives zero-valued PCM only during that idle grace period; this idle feed stops before speech is written. A short zero-valued drain follows each utterance, and MoHuddle waits for `mpv`'s device-adjusted audio position to reach the actual speech boundary before advancing the FIFO queue. If IPC cannot confirm that boundary while the player is still running, MoHuddle uses a conservative cancelable duration estimate, continues the FIFO queue, and keeps speech enabled; only an exited player or an audio write/device failure latches speech unavailable. Other responses therefore cannot overlap or lose the buffered end of an open raw stream. `/speak stop` and `/speak skip` terminate the active player immediately; a fresh player is created for later speech. If a Kokoro inference call is already running, it may finish silently before the next queued response starts; its output is discarded and no later sentence from the cancelled response is synthesized.

Under WSL, MoHuddle uses only WSLg's PulseAudio output rather than letting `mpv` fall through unavailable JACK and hardware ALSA devices. If WSLg audio is down, MoHuddle shows one concise error, clears queued speech, and pauses playback attempts while text chat continues. Confirm with `pactl info`. If it reports `Connection refused`, exit WSL applications and run `wsl --shutdown` from Windows PowerShell, reopen the distribution, and restart MoHuddle. This command stops every running WSL distribution, so save other WSL work first.

Voice mappings, selection, and the enabled state are personal settings and survive room changes and MoHuddle restarts. They are saved in the `speech` object of the personal configuration file. Advanced options should be edited while MoHuddle is stopped.

The legacy `edge` provider remains available for existing configurations by setting `provider` to `edge`; `playback_binary` is its optional executable override. MoHuddle never falls back from Kokoro to Edge automatically. Edge sends the normalized response text to Microsoft, whereas Kokoro made no runtime network calls in the WSL syscall trace after installation.

All executables receive separate process arguments rather than a shell command, and stopping speech terminates the player process group under Linux/WSL. Missing programs or models, invalid voices, inference failures, and audio failures are shown as nonfatal warnings and never interrupt text chat. A failed audio device is not retried for every queued response; use `/speak all` after the device has recovered. To remove the local runtime, stop MoHuddle and remove the exact `mohuddle/tts/kokoro` directory under the data path described above.

Kokoro's wrapper and ONNX Runtime declare permissive licenses, and its model weights are Apache 2.0. The user-local runtime also contains GPL-licensed phonemization components. MoHuddle does not bundle or redistribute those dependencies; review their licenses before packaging them with a binary.

## Filesystem access and approvals

The launch workspace is the initial read/write grant for all agents, but AGY and Copilot receive no roots while their effective turn is the built-in isolated read-only mode. In `workspace` or `full`, they receive the same workspace and approved directory grants as other workers. If a worker needs another directory, ask naturally—for example, `use ../shared-library as read-only context`. MoHuddle resolves and displays the canonical directory before granting access.

Directory approval choices are shown one at a time. If concurrent read-only agents request additional directories together, later requests wait in the UI queue:

- `y`: allow this request once.
- `a`: grant this agent access for the saved room.
- `b`: grant all agents access for the saved room.
- `n`: deny the request and allow the turn to finish.
- `x`: deny the request and stop the turn.

The provider mappings are:

- Codex uses its app-server `readOnly`, `workspaceWrite`, or `dangerFullAccess` sandbox policy. Workspace mode sets approval policy `never`, grants only approved roots, and disables network.
- Claude uses `plan`, `acceptEdits`, or `bypassPermissions`, plus its filesystem/network sandbox in read-only and workspace modes.
- AGY's built-in read-only turns run as direct, non-persistent sessions in an isolated temporary directory with slash expansion disabled, no original workspace roots, no auto-approved permissions, and the native terminal sandbox. The installed AGY CLI currently has an upstream print-mode custom-agent discovery bug, so MoHuddle does not rely on `--agent`; any emitted tool event fails the isolated turn closed. Workspace mode uses `accept-edits` with AGY's sandbox; full mode disables that sandbox.
- Copilot's built-in read-only turns use the official SDK in `ModeEmpty` with an explicit empty tool allowlist, no skills/config discovery, and no workspace roots. Any unexpected tool or access event fails the isolated turn closed. Workspace mode enables the explicit view, grep, edit, and shell tool set under MoHuddle's path policy; full mode enables all SDK tools.

Codex and Claude provide native OS-level sandbox controls in workspace mode. AGY uses its native terminal sandbox in workspace mode, while Copilot workspace mode relies on MoHuddle's SDK tool and path policy. Full mode disables those MoHuddle restrictions. Provider- or organization-managed policy may still impose additional restrictions.

Review every requested path and command before approving it. The AI providers still receive prompts and any file content their authenticated CLIs read as part of the work.

## Disagreements

Agents mark material disagreements about correctness, safety, implementation direction, or claimed results in private orchestration metadata. Peer disagreement returns to the moderator for resolution. Only an explicit unresolved disagreement from the moderator is saved and shown to you; a neutral incomplete response ends or advances the floor without opening the conflict dialog. Send new direction or use `/continue` to resume a real saved disagreement.

This is a conversational pause, not a pre-execution security gate. In `full` mode, it cannot prevent an action the agent already performed during its turn.

## Rooms, sessions, and local data

By default, data is stored under:

```text
$XDG_STATE_HOME/mohuddle
```

When `XDG_STATE_HOME` is unset, the default is:

```text
$HOME/.local/state/mohuddle
```

Each room contains metadata in `room.json` and an append-only `messages.jsonl` transcript. Directories use mode `0700`; files use mode `0600`. Room metadata includes the present/away roster, scheduled roster-action audit records, native provider session IDs and cursors, room settings, pending conflicts, and filesystem grants, but no provider credentials.

Back up the state directory if the transcripts matter to you. Remove a room only while MoHuddle is not running.

### Opening rooms created by the earlier `aichat` build

MoHuddle uses a separate state directory so both applications can coexist. To open the earlier application's rooms explicitly:

```bash
mohuddle --state-dir "${XDG_STATE_HOME:-$HOME/.local/state}/aichat"
```

Do not run MoHuddle and the earlier application against the same state directory simultaneously; room files do not use cross-process locking.

## Architecture

MoHuddle uses four provider adapters:

- The Codex adapter uses the official [Codex app-server protocol](https://learn.chatgpt.com/docs/app-server): JSONL over standard input/output, initialization, resumable threads, streamed events, approval requests, and turn interruption.
- The Claude adapter uses non-interactive print mode with streaming JSON and resumes the saved Claude session ID.
- The AGY adapter launches headless `agy` processes with streaming JSON; isolated read-only turns are disposable and reject any emitted tool event, while elevated direct turns use its worker modes.
- The Copilot adapter uses the official [GitHub Copilot SDK](https://github.com/github/copilot-sdk) for Go with a permission-dependent tool allowlist.

MoHuddle coordinates private lead bids, a sequential moderated floor, explicit parallel one-shots, optional-participant permission profiles, the public transcript, persistence, settings, approval queues, conflict pauses, activity indicators, optional queued speech, and TUI. Provider authentication, model access, quotas, managed policy, and billing remain the responsibility of the installed CLIs.

The API is split into a transport-neutral protocol/service layer, an OS-specific
local listener, and an explicitly enabled pinned-TLS federation listener. Both
call the same orchestrator operations as the TUI and receive independent event
subscriptions, so API consumers never steal TUI events or define a second room
behavior.

## Development

```bash
make build       # build bin/mohuddle
make test        # deterministic unit and integration tests with fake CLIs
make test-race   # run the suite with Go's race detector
make vet         # run static checks
make check       # test, race test, and vet
make live-test   # opt-in authenticated Codex/Claude integration test
```

The ordinary tests use fake CLIs and do not consume provider usage. `make live-test` sets `MOHUDDLE_LIVE=1`, invokes authenticated Codex and Claude, and may consume provider quota. AGY and Copilot adapters have deterministic protocol and permission tests; live provider use is exercised from the TUI.

GitHub Actions runs tests, the race detector, vet, and a build on pushes and pull requests. Tags matching `v*` create Linux `amd64` and `arm64` release archives with SHA-256 checksums.

## Current limitations

- A room is local to one operating-system user; coworkers cannot join the same live room remotely.
- A single room is still one conversation thread; there are no independently named or branching subthreads yet.
- Linux and WSL 2 are the supported release environments.
- Provider CLI protocol changes can require corresponding adapter updates.
