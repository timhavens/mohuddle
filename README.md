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
- Ordinary messages use a quiet, sequential Codex/Claude floor managed by a selected moderator.
- Codex and Claude privately assess task fit; MoHuddle selects the lead, then guarantees a read-only review by the other core agent.
- AGY and Copilot are optional participants that default to isolated read-only turns; explicit workspace or full permission enables their coding tools for direct work.
- Direct `@agent` messages invoke exactly that participant, without automatic review calls.
- Persistent activity rows show idle, queued, working, approval waits, errors, and elapsed work time while keeping tool chatter hidden by default. `/details on` reveals it.
- Independent, persistent model, reasoning-effort, and permission settings for every provider.
- Native provider session IDs and transcript cursors are saved and resumed. A returning agent catches up on messages sent while it was away.
- Public messages and concise tool summaries are stored in an append-only room transcript.
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

An ordinary message first obtains short, private task-fit bids from the present Codex and Claude workers. These transcript-only bids use disposable provider sessions with no workspace roots or tools, are not written to the room transcript, and cannot change saved provider sessions or cursors. When the bids agree, MoHuddle selects their preferred lead; a split or invalid result falls back to the room moderator—Codex by default.

Only one public participant works or speaks at a time. The selected lead answers or performs authorized work, then the other present core agent receives the completed response and reviews it read-only. If the moderator led, it gets a final read-only closing turn after the peer review. Marker-only reviews remain publicly silent. This guarantees that Codex and Claude both see ordinary requests without letting AGY and Copilot become routine background noise.

During its closing turn, the moderator may invite AGY or Copilot for a materially distinct perspective. Each may speak at most once and the floor then returns to the moderator. A neutral request for another response without a named participant automatically advances to the next eligible voice. Only an explicit material disagreement creates the conflict dialog; waiting, malformed routing, cancellation, or provider failure does not fabricate a conflict.

A targeted message such as `@codex implement this function` invokes only Codex with its configured permission profile. Common punctuation immediately after the name is accepted, so `@claude?` and `@claude, please review this` are also direct turns. `@agy` and `@copilot` bypass moderation. At their built-in read-only default those turns remain isolated and tool-free; setting either participant to `workspace` or `full` makes direct turns use its coding tools and saved native session. There is no automatic peer-review loop after a direct message.

Use `/moderator` to show the moderator or `/moderator @codex|@claude` to change it. If the moderator leaves, the other present core worker takes over automatically. Humans are never moderated: a new human message always steers the room immediately.

You can keep typing while agents work. Every new chat message is saved immediately, cancels the entire current workflow, and starts again from your latest steering. Non-empty public text that was streaming when cancellation occurred is stored in the transcript with an `interrupted` label; it does not advance that provider's saved transcript cursor. `/stop` cancels every active agent without starting another workflow.

For a discussion that should explicitly hear from the room, use `/round MESSAGE` for all present agents or select participants such as `/round @claude @agy MESSAGE`. Requested participants speak sequentially, all turns are read-only, individual failures do not prevent later speakers, and the moderator synthesizes last. For independent parallel answers with no synthesis, use `/ask MESSAGE` (or `/once`) or a selected subset such as `/ask @codex @agy MESSAGE`. These discussion workflows never use a saved workspace/full override; AGY and Copilot remain isolated and tool-free within them.

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

The composer keeps up to 200 submitted entries per room and restores them after a restart. Up and Down recall history for a single-line draft; Ctrl+P and Ctrl+N always move through history. MoHuddle preserves the unfinished draft, compact pasted blocks, and attached images while history is being browsed. Its compact, unnumbered input expands for multiline drafts. The context footer always shows the effective Codex and Claude model, effort, permission profile, and workspace; color highlights whichever core worker the current input targets.

Multiline or large pasted text is kept in full but displayed as a compact `Pasted Content` item until sent. Ctrl+V also checks the Windows image clipboard under WSL and displays a compact image item. Codex receives images through its native local-image input, Copilot through an SDK attachment, and Claude receives a private saved path it can read. AGY currently cannot inspect images; MoHuddle shows a warning and continues with the other selected participants. Room attachments and composer history are stored privately alongside room state rather than in the workspace.

PageUp/PageDown and Ctrl+Up/Ctrl+Down scroll the conversation without moving focus from the composer. Ctrl+Home/Ctrl+End jump to the beginning or end, and the mouse wheel scrolls the same viewport. New agent output no longer forces the screen to the bottom while you are reading earlier content; the footer reports how many new messages are waiting.

If MoHuddle exits while an agent is working, that turn is cancelled. Completed messages and non-empty interrupted public drafts remain saved, but unfinished work is not resumed automatically. Restart the room and use `/continue` or send the request again.

## Keyboard controls

```text
Enter       send the message
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
Esc         dismiss suggestions, otherwise stop active work
Ctrl+C      exit cleanly
```

Mouse scrolling is enabled by default. Press `Alt+M` to release mouse capture for normal drag selection, then press it again to restore mouse scrolling. In Windows Terminal, Shift+drag can also select text while mouse capture remains enabled.

When an approval dialog is visible, use the keys shown in the dialog instead of typing a chat message.

## Room commands

```text
/agents                    list supported agents as present, away, or unavailable
/moderator [@codex|@claude]
                           show or change the room moderator
/ask [@agent ...] MESSAGE  one concurrent response per selected/present agent
/round [@agent ...] MESSAGE
                           sequential read-only discussion with moderator synthesis
/join @agent|@all          add installed agent(s) to future rounds
/leave @agent|@all         remove installed agent(s) from future rounds
/continue                  start another bounded moderated round
/stop                      interrupt all active work
/details [on|off]          toggle or set behind-the-scenes tool/activity detail
/speak [on|off|all|@agent|stop|skip]
                           show or control spoken responses
/voice @agent [VOICE|off]  show, set, or clear an agent's voice
/voices [FILTER]           list available Edge voices
/status                    show the room, workspace, and native session IDs
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

AGY and Copilot default to `read-only`, which preserves their isolated transcript-only behavior with no tools or workspace roots. Set either to `workspace` or `full` when you want a direct `@agy` or `@copilot` turn to perform coding work; its native worker session and approved filesystem grants then persist normally. Set it back to `read-only` to restore isolation. Core review, moderator closing, `/round`, and `/ask` turns remain read-only discussion turns without changing any saved profile.

The first `full` selection requires typing `FULL ACCESS` exactly. The acknowledgement is saved in the personal settings file, so a saved full-access default starts without repeated confirmations. MoHuddle displays a red `FULL ACCESS` badge whenever a present agent uses this profile. Only enable it on a machine and in repositories you trust: a model mistake or prompt injection can read, change, transmit, or delete data before a conversational disagreement is detected.

Personal settings are stored at `$XDG_CONFIG_HOME/mohuddle/config.json`, falling back to `$HOME/.config/mohuddle/config.json`, with file mode `0600`.

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
- The AGY adapter launches headless `agy` processes with streaming JSON; isolated read-only turns are disposable and reject any emitted tool event, while elevated direct turns use its worker modes.
- The Copilot adapter uses the official [GitHub Copilot SDK](https://github.com/github/copilot-sdk) for Go with a permission-dependent tool allowlist.

MoHuddle coordinates private lead bids, a sequential moderated floor, explicit parallel one-shots, optional-participant permission profiles, the public transcript, persistence, settings, approval queues, conflict pauses, activity indicators, optional queued speech, and TUI. Provider authentication, model access, quotas, managed policy, and billing remain the responsibility of the installed CLIs.

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
