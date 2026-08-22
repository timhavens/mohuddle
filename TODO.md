# TODO

## Completed

### Terminal interface

- [x] Improve the MoHuddle terminal UI with Codex-style interaction conveniences.
  - [x] Add persistent per-room submitted-message history with Up/Down and Ctrl+P/Ctrl+N recall.
  - [x] Add keyboard and mouse conversation scrollback without losing the draft or composer focus.
  - [x] Add a runtime mouse-capture toggle so normal terminal text selection remains available without permanently giving up mouse scrolling.
  - [x] Preserve and restore an unfinished draft, pasted blocks, and images while browsing history.
  - [x] Add a compact context footer showing both core agents' model, effort, access, and workspace, with the current target highlighted.
  - [x] Add compact pasted-content/image items, slash-command discovery, shortcut hints, and new-message navigation cues.
  - [x] Pass image paths pasted or dragged into the composer through unchanged as message text, allowing capable agents to inspect files they can access.

## Future considerations

### Local-first speech providers and low-latency playback

Status: planning and evaluation only on `feature/local-tts-spike`. Do not merge
provider code to `main` until the decision gates below have been reviewed.

#### Goal and provider policy

Choose a trustworthy, low-latency speech provider using measurements from the
actual Debian/WSL host before replacing the current `edge-playback` path. Prefer
local synthesis so conversational responses, codebase details, and file paths do
not leave the machine merely to be spoken.

| Candidate | Data path | Initial posture |
| --- | --- | --- |
| Kokoro ONNX | Local after installation | First quality/latency spike; user-installed runtime and pinned model files |
| Piper | Local after installation | Comparison spike; user-installed executable and voice models |
| Edge TTS | Microsoft service | Existing behavior only; optional explicit cloud provider if retained |
| OpenAI TTS | OpenAI API | Possible paid opt-in provider later; not included with a ChatGPT/Codex subscription |

- There is no supported Claude/Anthropic general-purpose TTS API to integrate.
  Consumer-app voice modes are not MoHuddle provider contracts.
- Do not automatically fall back from a local provider to Edge or OpenAI. If the
  selected local provider is unavailable, speech becomes unavailable and text
  chat continues normally.
- A cloud provider, if retained, must be selected explicitly and clearly state
  that normalized response text leaves the machine. API credentials must come
  from environment/credential storage, never the room or personal config file.
- Start with one provider selected globally and stable per-agent voices within
  that provider. Mixed providers per agent are a later feature only if a real
  use case justifies the added configuration and lifecycle complexity.

#### Existing behavior to preserve

- `internal/speech.Service` owns one asynchronous FIFO queue, eligibility, and
  settings; agents never overlap during normal speech.
- Only completed AI `chat.MessageText` events are spoken. Live token deltas,
  tool output, interrupted drafts, user messages, and status events are not.
- Per-agent voice mappings, all/one-agent selection, enable/disable state,
  `/speak`, `/voice`, `/voices`, `Alt+V`, and the footer status remain available.
- Normalization creates a separate spoken copy. Stored and displayed messages
  are never modified.
- Provider startup, synthesis, playback, or cancellation failures remain
  nonfatal and cannot block MoHuddle startup, message delivery, or the TUI.

#### Proposed shared architecture

```text
completed AI MessageText
        -> SpeechService FIFO queue
        -> speech-only normalization
        -> sentence/safe-boundary utterance segments
        -> selected Synthesizer (persistent local worker where useful)
        -> typed audio stream (format + bytes)
        -> one AudioPlayer for the complete utterance
        -> speakers
```

- Keep queue ownership and UI events in Go. Provider-specific model loading,
  inference, and voice discovery stay behind a replaceable synthesizer boundary.
- Separate synthesis from playback rather than having every provider launch its
  own player. The stream must declare its audio format so the player can accept
  local PCM and any explicitly supported compressed cloud format.
- Change the current per-text-chunk `Provider.Play` loop to one service call per
  queued utterance. One player spans the response so sentence boundaries do not
  restart the audio device or introduce avoidable gaps.
- A queued utterance retains the source message ID, agent, resolved provider and
  voice, normalized segments, and enqueue time.
- Prefer a persistent local worker when model/session initialization materially
  affects latency. Worker startup and warmup are asynchronous and must not delay
  the TUI. The exact protocol—existing server API, framed stdin/stdout worker, or
  library integration—is selected by the spike, not assumed in advance.
- Initially locate and execute user-installed runtimes directly with separate
  process arguments and no shell. Do not bundle provider executables, model
  weights, phonemizers, or native libraries until their source, checksums,
  licenses, update story, and supported platforms have been reviewed.
- `/speak stop` cancels the current synthesis and playback and clears the queue.
  `/speak skip` cancels the current utterance only and begins the next. Disable
  and shutdown clean up the worker/player with bounded termination and no orphan
  processes.

#### Speech text policy

- Change inline-code normalization from omission to delimiter removal:
  `` `git status` `` is spoken as `git status`.
- Continue omitting fenced code, tables, JSON/structured blobs, terminal dumps,
  and stack traces, with at most one short on-screen-reference cue.
- Speak Markdown link labels but not raw destinations; remove heading/emphasis/
  list presentation markers and collapse excess whitespace.
- Split normalized prose at sentence or safe language boundaries for incremental
  synthesis. Do not treat the current `max_chunk_chars` value as a product-level
  truncation rule; provider token limits belong in the provider adapter.
- Add a distinct total `max_message_chars` only if desired. The recommended
  default above a total cap is to skip with a nonfatal diagnostic, never silently
  summarize or cut the agent's response.

#### Phase 0: reproducible provider spike

- [x] Build a standalone, disposable harness on the feature branch; do not wire
  an unvetted provider into completed room messages yet.
- [x] Test Kokoro and Piper with the same corpus: one sentence, approximately
  100 words, approximately 600 words, prose containing Markdown/inline code,
  and a rapid four-agent sequence using four distinct voices.
- [ ] Record exact runtime/model/voice versions, download origins, SHA-256
  checksums, install size, native dependencies, and licenses. Review the full
  phonemizer/runtime path—Kokoro weights alone do not describe its licensing or
  trust surface.
- [ ] Measure cold worker-ready time, warm time-to-first-audible-sound, synthesis
  real-time factor, CPU, peak resident memory, per-agent voice-switch overhead,
  long-response continuity, and stop/skip latency.
- [x] Verify both candidates synthesize the complete corpus after installation
  with their network namespace isolated.
- [ ] Trace or audit both runtime paths to confirm that they make no network
  attempt, including an attempted request that gracefully fails offline.
- [ ] Verify cancellation leaves no worker/player process and the next queued
  request can run.
- [ ] Perform a listening comparison for intelligibility, naturalness, technical
  terms, punctuation, voice distinctness, clicks/gaps between sentences, and
  fatigue over a long response.

Proposed evaluation gates (adjust after the first baseline run):

- Warm first audible speech targets 750 ms or less on the known WSL host and
  must materially improve on the current Edge baseline.
- Sustained synthesis targets a real-time factor below 0.5 so generation remains
  comfortably ahead of playback.
- Stop/skip targets complete synthesis/player cancellation within one second.
- Four configured agent voices must not require avoidable model reloads between
  queued utterances; document memory tradeoffs if a candidate uses one model per
  voice.
- The chosen provider must have an acceptable supply-chain, maintenance, and
  license posture for user-installed execution. Bundling is a separate decision.

Current measured candidate: Piper 1.7.0 with the CC BY 4.0
`en_US-libritts_r-medium` multi-speaker model. One 75 MiB ONNX session supplies
all four agent voices, produces first PCM in 51–199 ms, and retains about 427 MiB
after the long case with the default arena (about 252 MiB with the arena disabled).
This is a benchmark leader, not a selection; listening, audible latency,
cancellation, runtime-network audit, and GPL/user-install posture remain gates.

#### Decision gate

- [ ] Record the spike results in the repository and select one of: Kokoro local,
  Piper local, neither local candidate, or more investigation required.
- [ ] Do not select solely from published quality claims or repository popularity.
  Use the WSL measurements, listening results, dependency audit, and maintenance
  risk together.
- [ ] If neither local candidate passes, decide explicitly whether to retain the
  current non-streaming Edge path temporarily, implement opt-in Edge streaming,
  evaluate paid OpenAI TTS, or leave speech unavailable. Do not silently convert
  a failed local selection into cloud egress.

#### Phase 1: integrate the selected local provider

- [ ] Introduce the synthesizer/audio-player boundaries and migrate the current
  Edge-specific provider without changing queue or UI behavior.
- [ ] Add provider selection and provider-specific paths/model settings with a
  settings-version migration. Preserve existing voice mappings where names are
  still valid; otherwise report that remapping is required.
- [ ] Start, validate, warm, monitor, and stop the selected local worker without
  blocking MoHuddle. Surface concise unavailability/failure diagnostics.
- [ ] Feed sentence-level audio into one player per utterance so speech can begin
  before the full response is synthesized and PCM remains continuous across
  segment boundaries.
- [ ] Carry timing diagnostics: T0 enqueue/provider request, T1 first audio from
  synthesizer, T2 first player write, T2b player-start/audio-output-ready proxy,
  T3 synthesis complete, and T4 playback complete. Keep routine UI output quiet.
- [ ] Correct normalization and add regression cases for inline backticks,
  fenced code, Markdown, links, tables, JSON, terminal output, stack traces, and
  mixed prose.
- [ ] Add deterministic service/provider/player tests for FIFO non-overlap,
  voice selection, continuous multi-segment playback, failure isolation,
  stop/skip, shutdown, and process cleanup. Keep a manual WSL audio checklist.
- [ ] Update README installation, configuration, privacy, license, model-source,
  troubleshooting, and removal instructions only after a provider is selected.

#### Optional cloud work after the local decision

- [ ] If Edge remains desirable, revive the Edge streaming spike as a separate
  opt-in adapter: `edge_tts.Communicate.stream()` into one player, no temporary
  MP3, explicit network-failure handling, and real time-to-first-sound metrics.
- [ ] First test whether one Edge `Communicate` call handles realistic long
  utterances. If multiple independent MP3 streams are required, test real audio
  across concatenation boundaries for clicks, gaps, and timestamp resets.
- [ ] If OpenAI TTS is desired, design it as a separately billed API provider
  with explicit credential and data-egress configuration. Do not present it as a
  voice entitlement supplied by Codex or a ChatGPT subscription.
- [ ] Cloud adapters never become automatic fallback behavior.

#### Initial acceptance criteria

- [ ] A reviewed local provider runs without network access after installation,
  passes the agreed latency/quality gates, and supports stable distinct agent
  voices.
- [ ] Audible speech begins before the complete response is synthesized, using
  one continuous player per utterance with no audible segment seams.
- [ ] Multiple completed responses remain FIFO and never overlap.
- [ ] Inline backticked text is spoken; full technical blocks are not read aloud;
  the stored/displayed response remains byte-for-byte unchanged.
- [ ] Stop and skip cancel both synthesis and playback within the agreed bound,
  leave no orphan process, and preserve correct queue semantics.
- [ ] Missing dependencies, invalid voices, provider errors, and audio failures
  are nonfatal; text chat and later queue items continue normally.
- [ ] Existing speech controls and persistence remain compatible, and operation
  with speech disabled is unchanged.
- [ ] No cloud provider receives response text unless the user explicitly selects
  it, and local failure never enables cloud fallback.
- [ ] No runtime/model dependency is bundled or redistributed without a recorded
  source, checksum strategy, license review, and maintenance decision.

### Voice participants

- [ ] Consider allowing AGY and Copilot to receive explicitly selected files as read-only context while preserving their voice-only, non-mutating role.
  - Prefer per-message file delivery rather than filesystem or directory grants.
  - Copilot can currently receive clipboard images through its SDK attachment mechanism, but it cannot open a path supplied as message text.
  - AGY currently cannot inspect image attachments or files referenced by path.
