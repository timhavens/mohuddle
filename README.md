# MoHuddle

MoHuddle is a local terminal room where you, OpenAI Codex, and Anthropic Claude Code can discuss and work on the same project. It launches the Codex and Claude command-line tools already installed on your computer, streams their public responses and tool activity into one TUI, and keeps you in the conversation.

```text
You ─┬─ Codex CLI  ── OpenAI
     └─ Claude CLI ── Anthropic
```

MoHuddle does not call the OpenAI or Anthropic HTTP APIs directly and does not store provider credentials or API keys. Each CLI connects with the account and authentication method configured by its user.

## Features

- One terminal conversation shared by you, Codex, and Claude.
- Ordinary messages start bounded, alternating Codex/Claude rounds.
- `@codex` and `@claude` send a message to only one agent.
- Persistent activity rows show idle, queued, thinking, streaming, tool use, approval waits, errors, and elapsed work time.
- Native Codex thread and Claude session IDs are saved and resumed.
- Public messages and concise tool summaries are stored in an append-only room transcript.
- Filesystem grants and approval prompts keep additional directory access explicit.
- Only one agent works at a time, reducing conflicting edits in a shared workspace.

## Supported environment

MoHuddle currently targets Linux, including Linux distributions running under WSL 2. Release builds are produced for Linux `amd64` and `arm64`.

Runtime requirements:

- A terminal with color and alternate-screen support.
- The [Codex CLI](https://learn.chatgpt.com/docs/codex/cli), installed and authenticated.
- [Claude Code](https://code.claude.com/docs/en/getting-started), installed and authenticated.
- [Bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) for Claude's Linux sandbox.
- Internet access for the Codex and Claude CLIs.
- Access to Codex and Claude through the accounts configured in those CLIs.

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

Confirm the complete runtime before starting MoHuddle:

```bash
codex --version
codex login status
claude --version
claude auth status
bwrap --version
```

Each coworker should authenticate with their own provider accounts. Do not copy or share CLI credential files.

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
```

## Conversation behavior

An ordinary message starts an alternating Codex/Claude round. The opening agent alternates between rounds, and the round stops when both agents report completion or the configured turn limit is reached.

A targeted message such as `@claude review this function` runs only that agent. A new message targeting the currently active agent interrupts and restarts that agent with the new transcript. A new untargeted message interrupts active work and starts a fresh round.

The agents receive the public room transcript and concise tool summaries. Their hidden reasoning is neither displayed nor copied between providers.

The activity rows remain visible even before response text arrives:

```text
⠹ CODEX   thinking    12s  · waiting for model response
○ CLAUDE  idle             · response posted
```

If MoHuddle exits while an agent is working, that turn is cancelled. Completed messages remain saved, but unfinished streamed text is not resumed automatically. Restart the room and use `/continue` or send the request again.

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
/continue                  run another bounded debate round
/stop                      interrupt active work
/status                    show the room, workspace, and native session IDs
/access                    show filesystem grants for the room
/revoke [@codex|@claude|@all] PATH
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
--max-turns N              cap an untargeted round; default 4
--codex-binary PATH        use a non-default Codex executable
--claude-binary PATH       use a non-default Claude executable
--codex-model MODEL        override the Codex CLI model
--claude-model MODEL       override the Claude CLI model
--state-dir PATH           override local room storage
--version                  print the MoHuddle version
```

`--room` and `--new` cannot be used together.

## Filesystem access and approvals

The launch workspace starts with read/write access for both agents. If an agent needs another directory, ask naturally—for example, `use ../shared-library as read-only context`. MoHuddle resolves and displays the canonical directory before granting access.

Directory approval choices are:

- `y`: allow this request once.
- `a`: grant this agent access for the saved room.
- `b`: grant both agents access for the saved room.
- `n`: deny the request and allow the turn to finish.
- `x`: deny the request and stop the turn.

Codex runs through its app-server `workspaceWrite` sandbox with network disabled for the turn and forwards native command/file approvals to the TUI. Claude runs with its Linux sandbox enabled and configured to fail closed if sandboxing is unavailable. MoHuddle does not use a skip-permissions mode for either agent.

Review every requested path and command before approving it. The AI providers still receive prompts and any file content their authenticated CLIs read as part of the work.

## Rooms, sessions, and local data

By default, data is stored under:

```text
$XDG_STATE_HOME/mohuddle
```

When `XDG_STATE_HOME` is unset, the default is:

```text
$HOME/.local/state/mohuddle
```

Each room contains metadata in `room.json` and an append-only `messages.jsonl` transcript. Directories use mode `0700`; files use mode `0600`. Room metadata includes native provider session IDs and filesystem grants, but no provider credentials.

Back up the state directory if the transcripts matter to you. Remove a room only while MoHuddle is not running.

### Opening rooms created by the earlier `aichat` build

MoHuddle uses a separate state directory so both applications can coexist. To open the earlier application's rooms explicitly:

```bash
mohuddle --state-dir "${XDG_STATE_HOME:-$HOME/.local/state}/aichat"
```

Do not run MoHuddle and the earlier application against the same state directory simultaneously; room files do not use cross-process locking.

## Architecture

MoHuddle launches both provider CLIs as child processes:

- The Codex adapter uses the official [Codex app-server protocol](https://learn.chatgpt.com/docs/app-server): JSONL over standard input/output, initialization, resumable threads, streamed events, approval requests, and turn interruption.
- The Claude adapter uses non-interactive print mode with streaming JSON and resumes the saved Claude session ID.

MoHuddle coordinates the public transcript, turn order, persistence, approvals, activity indicators, and TUI. Provider authentication, model access, quotas, and billing remain the responsibility of the installed CLIs.

## Development

```bash
make build       # build bin/mohuddle
make test        # deterministic unit and integration tests with fake CLIs
make test-race   # run the suite with Go's race detector
make vet         # run static checks
make check       # test, race test, and vet
make live-test   # opt-in authenticated Codex/Claude integration test
```

The ordinary tests do not consume provider usage. `make live-test` sets `MOHUDDLE_LIVE=1`, invokes both authenticated CLIs, and may consume provider quota.

GitHub Actions runs tests, the race detector, vet, and a build on pushes and pull requests. Tags matching `v*` create Linux `amd64` and `arm64` release archives with SHA-256 checksums.

## Current limitations

- A room is local to one operating-system user; coworkers cannot join the same live room remotely.
- Codex and Claude are currently the only agent adapters.
- Agent turns are serialized rather than concurrent.
- Linux and WSL 2 are the supported release environments.
- Provider CLI protocol changes can require corresponding adapter updates.
