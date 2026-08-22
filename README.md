# MoHuddle

MoHuddle is a local terminal room where you and multiple coding agents can discuss and work on the same project. It supports OpenAI Codex, Anthropic Claude Code, Google Antigravity (`agy`), and GitHub Copilot. MoHuddle launches the providers' command-line tools already installed on your computer, streams their public responses and tool activity into one TUI, and keeps you in the conversation.

```text
You ─┬─ Codex CLI   ── OpenAI
     ├─ Claude CLI  ── Anthropic
     ├─ AGY CLI     ── Google
     └─ Copilot CLI ── GitHub
```

MoHuddle does not call provider model APIs directly and does not store provider credentials or API keys. Each CLI or its official SDK transport connects with the account and authentication method configured by its user.

## Features

- One terminal conversation shared by you and any combination of Codex, Claude, AGY, and Copilot.
- New rooms start with Codex and Claude present. `/join` and `/leave` change the roster and save it with the room.
- Ordinary messages use a quiet, sequential floor managed by a selected Codex or Claude moderator.
- Codex and Claude privately assess task fit before each ordinary message; the moderator chooses the lead, can work directly, or delegates.
- AGY and Copilot are voice-only participants: they can reason from the room transcript but have no tools, workspace access, or editor role.
- Direct `@agent` messages invoke exactly that participant, without automatic review calls.
- Persistent activity rows show idle, queued, working, approval waits, errors, and elapsed work time while keeping tool chatter hidden by default. `/details on` reveals it.
- Independent, persistent model and reasoning-effort settings for every provider, plus worker permission profiles for Codex and Claude.
- Native provider session IDs and transcript cursors are saved and resumed. A returning agent catches up on messages sent while it was away.
- Public messages and concise tool summaries are stored in an append-only room transcript.
- Filesystem grants and approval prompts keep additional directory access explicit.
- `/ask [@agent ...] MESSAGE` retains explicit one-shot parallel participation when it is useful.
- Moderated rounds are structurally bounded: each non-moderator may be invited at most once before the floor closes or returns to you.

## Supported environment

MoHuddle currently targets Linux, including Linux distributions running under WSL 2. Release builds are produced for Linux `amd64` and `arm64`.

Runtime requirements:

- A terminal with color and alternate-screen support.
- The [Codex CLI](https://learn.chatgpt.com/docs/codex/cli), installed and authenticated.
- [Claude Code](https://code.claude.com/docs/en/getting-started), installed and authenticated.
- [Bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) for Claude's Linux sandbox.
- Optionally, [Google Antigravity CLI](https://www.agy.dev/docs/cli/getting-started/) and/or [GitHub Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/install-copilot-cli), installed and authenticated before joining them to a room.
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
```

## Conversation behavior

An ordinary message first obtains short, private task-fit bids from the present Codex and Claude workers. These transcript-only bids use disposable provider sessions with no workspace roots or tools, are not written to the room transcript, and cannot change the saved provider sessions or cursors. The room moderator—Codex by default—uses both bids to decide whether to handle the request directly or give another participant the floor.

Only one public participant works or speaks at a time. After a delegated participant responds, the floor returns to the moderator. The moderator remains publicly silent when the response is sufficient; it speaks only to correct, synthesize, delegate again, or surface a disagreement. Each non-moderator can be invited once per normal round, so AGY and Copilot can both contribute when they have materially different perspectives without becoming routine background noise.

A targeted message such as `@codex implement this function` invokes only Codex with its configured worker permission profile. `@agy` and `@copilot` also bypass moderation, but their turns remain transcript-only and tool-free. There is no automatic peer-review loop after any direct message.

Use `/moderator` to show the moderator or `/moderator @codex|@claude` to change it. If the moderator leaves, the other present core worker takes over automatically. Humans are never moderated: a new human message always steers the room immediately.

You can keep typing while agents work. Every new chat message is saved immediately, cancels the entire current workflow, and starts again from your latest steering. Non-empty public text that was streaming when cancellation occurred is stored in the transcript with an `interrupted` label; it does not advance that provider's saved transcript cursor. `/stop` cancels every active agent without starting another workflow.

For a question that benefits from independent parallel answers, use `/ask MESSAGE` (or `/once`) for all present agents, or select a subset such as `/ask @codex @agy MESSAGE`. Each selected participant responds once with no review wave or moderator synthesis. Codex and Claude are read-only for these calls; AGY and Copilot remain tool-free.

Provider calls use one execution lane per agent. Codex, Claude, AGY, and Copilot can therefore overlap with one another, while MoHuddle never starts two simultaneous calls against the same provider session.

Use `/agents` to see which CLIs MoHuddle found and which agents are present. `/leave @agent` removes an idle agent from future rounds; `/join @agent` returns it. Roster changes are accepted only when no round is active. They are written to `room.json`, do not delete the provider session or cursor, and are restored after restarting MoHuddle. On its next turn, a returning agent receives the room messages added since its last completed response. Only commands you type change membership; an AI cannot remove another participant.

The agents receive the room transcript, including stored tool summaries and interrupted drafts. Their hidden reasoning is neither displayed nor copied between providers.

Routing, task-fit bids, and sufficient moderator closings stay private. Marker-only completions are not written to the public transcript, and agents are instructed not to post filler such as “no disagreement,” “nothing to add,” or “standing by.”

The quiet activity rows remain visible even before response text arrives:

```text
⠹ CODEX   working  12s
○ CLAUDE  idle
○ AGY     away
○ COPILOT idle
```

Public response text still streams into the conversation as it arrives. Tool names, commands, paths, status chatter, and compact model/settings labels are hidden in quiet mode. `/details` toggles the personal setting, while `/details on` and `/details off` set it explicitly. Turning details on reveals historical tool messages already stored in the room as well as new activity.

If MoHuddle exits while an agent is working, that turn is cancelled. Completed messages and non-empty interrupted public drafts remain saved, but unfinished work is not resumed automatically. Restart the room and use `/continue` or send the request again.

## Keyboard controls

```text
Enter       send the message
Alt+Enter   insert a newline
Esc         stop active work
Ctrl+C      exit cleanly
```

When an approval dialog is visible, use the keys shown in the dialog instead of typing a chat message.

## Room commands

```text
/agents                    list supported agents as present, away, or unavailable
/moderator [@codex|@claude]
                           show or change the room moderator
/ask [@agent ...] MESSAGE  one concurrent response per selected/present agent
/join @agent|@all          add installed agent(s) to future rounds
/leave @agent|@all         remove installed agent(s) from future rounds
/continue                  start another bounded moderated round
/stop                      interrupt all active work
/details [on|off]          toggle or set behind-the-scenes tool/activity detail
/status                    show the room, workspace, and native session IDs
/settings                  show effective settings, personal defaults, and command examples
/models @agent             list that provider's selectable models and effort levels
/model [default] @agent|@all MODEL
/effort [default] @agent|@all LEVEL
/permissions [default] @codex|@claude|@all PROFILE
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
--agy-permissions NAME     deprecated; AGY is always voice-only
--copilot-permissions NAME deprecated; Copilot is always voice-only
--state-dir PATH           override local room storage
--config PATH              override the personal settings file
--version                  print the MoHuddle version
```

`--room` and `--new` cannot be used together. The old wave/turn cap options remain parseable for compatibility but do not control the structurally bounded moderated workflow.

## Models, effort, and permissions

Models and effort are configured independently for every provider. Permission profiles apply only to the Codex and Claude core workers. Effective settings use this precedence:

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
- `workspace`: the selected agents can edit and run commands without routine approvals inside granted roots; network access is blocked where the provider transport can enforce it. This is the built-in default.
- `full`: provider approvals and MoHuddle sandboxes are disabled, giving that agent unrestricted host filesystem and network access.

These profiles apply when Codex or Claude handles a normal moderated task or a direct message. Explicit `/ask` turns force both core workers read-only without changing their saved profiles. AGY and Copilot always display `voice-only (no tools)` and reject permission changes.

The first `full` selection requires typing `FULL ACCESS` exactly. The acknowledgement is saved in the personal settings file, so a saved full-access default starts without repeated confirmations. MoHuddle displays a red `FULL ACCESS` badge whenever a present agent uses this profile. Only enable it on a machine and in repositories you trust: a model mistake or prompt injection can read, change, transmit, or delete data before a conversational disagreement is detected.

Personal settings are stored at `$XDG_CONFIG_HOME/mohuddle/config.json`, falling back to `$HOME/.config/mohuddle/config.json`, with file mode `0600`.

## Filesystem access and approvals

The launch workspace starts with read/write access for Codex and Claude. AGY and Copilot receive no workspace roots and cannot request grants. If a core worker needs another directory, ask naturally—for example, `use ../shared-library as read-only context`. MoHuddle resolves and displays the canonical directory before granting access.

Directory approval choices are shown one at a time. If concurrent read-only agents request additional directories together, later requests wait in the UI queue:

- `y`: allow this request once.
- `a`: grant this agent access for the saved room.
- `b`: grant all core workers access for the saved room.
- `n`: deny the request and allow the turn to finish.
- `x`: deny the request and stop the turn.

The provider mappings are:

- Codex uses its app-server `readOnly`, `workspaceWrite`, or `dangerFullAccess` sandbox policy. Workspace mode sets approval policy `never`, grants only approved roots, and disables network.
- Claude uses `plan`, `acceptEdits`, or `bypassPermissions`, plus its filesystem/network sandbox in read-only and workspace modes.
- AGY voice turns run in an isolated temporary directory using a generated custom agent with `tools: []`, no original workspace roots, no resumed work session, and the native terminal sandbox. Any unexpected tool event fails the turn closed.
- Copilot voice turns use the official SDK in `ModeEmpty` with an explicit empty tool allowlist, no skills/config discovery, and no workspace roots. Any unexpected tool or access event fails the turn closed.

Codex and Claude provide native OS-level sandbox controls in workspace mode. AGY and Copilot are not offered a work mode, so their former workspace/full shell-policy caveats no longer apply inside MoHuddle. Provider- or organization-managed policy may impose additional restrictions.

Review every requested path and command before approving it. The AI providers still receive prompts and any file content their authenticated CLIs read as part of the work.

## Disagreements

Agents mark material disagreements about correctness, safety, implementation direction, or claimed results in private orchestration metadata. The floor returns to the moderator to resolve or explain them. If the moderator ends without resolution, MoHuddle saves the reason and waits for you. Send a new message to provide direction, or use `/continue` to start another bounded moderated round.

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

Each room contains metadata in `room.json` and an append-only `messages.jsonl` transcript. Directories use mode `0700`; files use mode `0600`. Room metadata includes the present/away roster, native provider session IDs and cursors, room settings, pending conflicts, and filesystem grants, but no provider credentials.

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
- The AGY adapter launches headless `agy` processes with streaming JSON; voice turns use disposable, tool-free custom-agent sessions.
- The Copilot adapter uses the official [GitHub Copilot SDK](https://github.com/github/copilot-sdk) for Go with an empty tool allowlist.

MoHuddle coordinates private lead bids, a sequential moderated floor, explicit parallel one-shots, fixed worker/voice capabilities, the public transcript, persistence, settings, approval queues, conflict pauses, activity indicators, and TUI. Provider authentication, model access, quotas, managed policy, and billing remain the responsibility of the installed CLIs.

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
