# TODO

## Requested features

### Acknowledged AI-to-AI correction statistics

- [x] Extend the existing private control marker with sequence references for an
  AI offering a material correction, the target accepting or disputing it, and
  the proposer retracting it. Do not infer correction events from prose.
- [x] Derive the target from the referenced public AI message, limit references
  to the transcript supplied to that participant, and validate every transition
  at the host boundary. Structurally reject user messages, self-corrections,
  marker-only claims, duplicate resolutions, and unauthorized actors; instruct
  agents not to declare additions, stylistic suggestions, or ordinary
  disagreements as corrections.
- [x] Treat accepted and retracted corrections as terminal. A target may
  dispute a correction, but it remains pending until the target accepts it or
  the proposer retracts it; targets cannot unilaterally penalize proposers.
- [x] Persist immutable correction events atomically with their public transcript
  messages and derive room/per-AI counts by deterministic sequence-order replay
  for offered, accepted, retracted, and pending corrections plus acknowledged
  corrections received. A new room begins with an empty ledger.
- [x] Surface room totals and every AI's counts in `/status`, without presenting
  them as a reliability or quality score.
- [x] Test marker parsing, attribution, lifecycle transitions, invalid and
  duplicate references, derived statistics, restart persistence, and status
  presentation.

### Add a Plan only mode

### Configurable core peers and availability failover

- [x] Separate provider identity, room presence, core/optional scheduling role,
  moderator eligibility, permission profile, model, and native session. Changing
  one concern must not silently change or copy another.
- [x] Keep Codex and Claude as the built-in preferred core peers, with AGY and
  Copilot optional by default, while allowing users to configure a different
  preferred core set and fallback order globally or per room.
- [x] Persist the preferred core roster independently from temporary promotions
  so restart, room resume, and later restoration preserve the user's intent.
- [x] Give every active core peer the same orchestration contract regardless of
  provider: eligibility to bid, lead, review, moderate, resolve disagreements,
  and receive normal follow-up turns. Do not impose smaller invitation counts or
  turn budgets merely because the participant is AGY or Copilot.
- [x] Generalize lead bidding and validation to the current active core roster
  instead of hard-coding `codex|claude`, including correct behavior with one,
  two, or more active core peers.
- [x] Permit any present active core peer to moderate. If the moderator becomes
  unavailable, select an eligible replacement using the configured fallback
  order while preserving an explicit user override when possible.
- [x] Detect when a preferred core peer is unavailable or in a confirmed provider
  cooldown and temporarily promote an available fallback. Prefer automatic
  failover, with room settings to require confirmation or disable it.
- [x] Model promotion as a temporary scheduling-role overlay. A promoted peer
  retains its own identity, model, effort, permissions, grants, transcript cursor,
  and native provider session; it never inherits those of the unavailable peer.
- [x] Keep permission policy independent from peer role. Promotion grants normal
  core participation but does not grant filesystem, tool, or network access; the
  promoted peer continues using its configured permission profile.
- [x] Support manual commands to inspect and configure preferred cores and
  fallbacks, promote or demote a participant, replace a specific unavailable
  core, restore the preferred roster, and control automatic failover.
- [x] Allow more than one fallback to be promoted when desired and define
  deterministic behavior when a preferred fallback is also unavailable.
- [x] Restore a recovered preferred core at the next safe workflow boundary,
  never in the middle of another participant's turn. Make restoration policy
  configurable as automatic, prompted, or manual.
- [x] Show preferred cores, active cores, temporary promotions, unavailable peers,
  current moderator, and pending restoration in `/agents`, `/status`, and the TUI.
- [x] Migrate existing rooms to the Codex/Claude preferred-core default without
  changing membership, permissions, sessions, or current user settings.
- [x] Test custom core rosters, one and multiple promoted fallbacks, moderator
  failure, restart persistence, manual overrides, provider cooldown and recovery,
  substitute failure, safe-boundary restoration, and equal lead/review treatment
  across all supported providers.
- [ ] Design a host-mediated roster-action request for cases such as scheduling a
  limited peer to rejoin after its retry time. An AI must never execute another
  participant's `/join` or `/leave` command from transcript text; any scheduled
  action needs explicit user authorization, persistence, cancellation, and an
  audit record.

### Local API and MoHuddle federation

- [ ] Define one versioned command-and-event protocol for joining rooms, reading
  history, sending messages, invoking commands, observing agent activity, and
  streaming results.
- [ ] Serve local clients through an OS-protected Unix socket or named pipe. Any
  loopback HTTP/WebSocket adapter must also authenticate clients.
- [ ] Assign every client and instance an immutable, namespaced identity with
  scoped permissions and an auditable connection history.
- [ ] Support explicit peer pairing between MoHuddle instances; do not permit
  unauthenticated discovery or automatic LAN joining.
- [ ] Include globally unique message IDs, origin and route metadata, bounded hop
  counts, deduplication, and loop prevention. Add causal ordering only if later
  use cases require it.
- [ ] Treat federated participants as restricted guests by default. A remote peer
  must never inherit local filesystem or execution permissions implicitly.
- [ ] Support hosted services such as ChatGPT through an explicit local bridge,
  connector, or outbound relay; do not assume a hosted client can contact
  localhost directly.
- [ ] Separate the protocol from its transports so terminal clients,
  integrations, phone access, and federation share the same behavior.

### Secure remote phone access

- [ ] Build a mobile-friendly web client or PWA on the shared MoHuddle API before
  considering separate native applications.
- [ ] Keep remote access disabled by default and avoid exposing an
  unauthenticated public listener. Support encrypted private-network, tunnel, or
  explicitly configured gateway access.
- [ ] Add an intentional host-side pairing flow using a short-lived code or QR
  exchange. Pairing creates a device identity key and revocable credentials;
  ordinary access sessions should expire independently.
- [ ] Provide separate `observe`, `participate`, and `admin` scopes. Because a
  remote message can cause an elevated agent to execute tools, the host must also
  define the maximum permission level that remote requests may trigger.
- [ ] Default newly paired devices to the least privilege selected by the host,
  with elevation and revocation controlled from the trusted local TUI.
- [ ] Resume from a transcript/event cursor after reconnect rather than relying
  on a fixed message count. Bound queued data and report gaps explicitly.
- [ ] Show connection identity, permission scope, elevated actions,
  authentication failures, and revoked devices in the local audit/status view.

### Provider cooldowns and temporary substitution

- [ ] Represent availability as structured state containing the participant,
  reason, source, detection time, retry time, and confidence.
- [ ] Detect structured provider quota, session-limit, and rate-limit signals
  where adapters expose them. Treat parsed free-form reset times conservatively
  and request confirmation when the timestamp or timezone is uncertain.
- [ ] Stop dispatching turns to a participant during a confirmed cooldown while
  preserving its roster membership, native session, transcript cursor, settings,
  and grants.
- [ ] Add per-room substitution policy: `off`, `prompt`, or `auto`, defaulting to
  `prompt`. A prompt must not block the room indefinitely.
- [ ] Model substitution as temporary scheduling/role routing, not participant
  replacement. The substitute retains its own identity, model, session, and
  permissions and never inherits the unavailable participant's access.
- [ ] Allow explicit preferred substitutes and define behavior when the
  substitute is also unavailable.
- [ ] Make an expired provider eligible again at the next safe workflow boundary.
  Never interrupt an active substitute turn or silently discard work.
- [ ] Surface cooldowns, substitutes, reset times, and restoration decisions in
  `/status`, with commands to set, clear, extend, replace, or restore them
  manually.
- [ ] Test restart persistence, uncertain reset times, clock/timezone handling,
  repeated failures, substitute failure, manual override, and restoration without
  session loss.

## Completed

### Terminal interface

- [x] Improve the MoHuddle terminal UI with Codex-style interaction conveniences.
  - [x] Add persistent per-room submitted-message history with Up/Down and Ctrl+P/Ctrl+N recall.
  - [x] Add keyboard and mouse conversation scrollback without losing the draft or composer focus.
  - [x] Add a runtime mouse-capture toggle so normal terminal text selection remains available without permanently giving up mouse scrolling.
  - [x] Preserve and restore an unfinished draft, pasted blocks, and images while browsing history.
  - [x] Add a compact context footer showing the active core peers' model, effort, access, and workspace, with the current targets highlighted.
  - [x] Add compact pasted-content/image items, slash-command discovery, shortcut hints, and new-message navigation cues.
  - [x] Pass image paths pasted or dragged into the composer through unchanged as message text, allowing capable agents to inspect files they can access.

## Future considerations

### Local-first speech providers and low-latency playback

Status: Kokoro selected and integrated on `feature/local-tts-spike`;
shared-model Piper remains the measured fallback. Real-device playback,
multi-agent FIFO/seams, and stop/skip controls passed user acceptance on WSLg.

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
        -> one persistent AudioPlayer across normal utterances
        -> speakers
```

- Keep queue ownership and UI events in Go. Provider-specific model loading,
  inference, and voice discovery stay behind a replaceable synthesizer boundary.
- Separate synthesis from playback rather than having every provider launch its
  own player. The stream must declare its audio format so the player can accept
  local PCM and any explicitly supported compressed cloud format.
- Change the current per-text-chunk `Provider.Play` loop to one service call per
  queued utterance. Keep the local player open across normal utterances so
  sentence and response boundaries do not restart the audio device. An explicit
  stop/skip may terminate and recreate it to flush already-buffered audio.
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
- `/speak stop` stops playback immediately, marks the utterance cancelled, and
  clears the queue. `/speak skip` does the same for the current utterance and
  begins the next. With synchronous local inference, the current segment
  may finish silently; discard its output and never start another segment. Keep
  the warm worker unless it faults or MoHuddle shuts down.

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
- Keep individual synthesis calls reasonably sized independently of the total
  message, but do not claim a character-count cancellation bound. Character
  count is only a coarse latency proxy; phoneme count and repeated observed call
  latency are better inputs if a stricter segment policy is needed later.
- Add a distinct total `max_message_chars` only if desired. The recommended
  default above a total cap is to skip with a nonfatal diagnostic, never silently
  summarize or cut the agent's response.

#### Phase 0: reproducible provider spike

- [x] Build a standalone, disposable harness on the feature branch; do not wire
  an unvetted provider into completed room messages yet.
- [x] Test Kokoro and Piper with the same corpus: one sentence, approximately
  100 words, approximately 600 words, prose containing Markdown/inline code,
  and a rapid four-agent sequence using four distinct voices.
- [x] Record exact runtime/model/voice versions, download origins, SHA-256
  checksums, install size, native dependencies, and licenses. Review the full
  phonemizer/runtime path—Kokoro weights alone do not describe its licensing or
  trust surface.
- [ ] Measure cold worker-ready time, warm time-to-first-audible-sound, synthesis
  real-time factor, CPU, peak resident memory, per-agent voice-switch overhead,
  long-response continuity, and stop/skip latency.
- [x] Verify both candidates synthesize the complete corpus after installation
  with their network namespace isolated.
- [x] Trace or audit both runtime paths to confirm that they make no network
  attempt, including an attempted request that gracefully fails offline.
- [x] Verify stop/skip halts playback within one second, discards the in-flight
  synchronous segment, starts no later segment, and allows the next queued
  request to run when the active call returns. A healthy warm worker may remain.
- [x] Perform a listening comparison for intelligibility, naturalness, technical
  terms, punctuation, voice distinctness, clicks/gaps between sentences, and
  fatigue over a long response.

Proposed evaluation gates (adjust after the first baseline run):

- Warm first audible speech targets 750 ms or less on the known WSL host and
  must materially improve on the current Edge baseline.
- Sustained synthesis targets a real-time factor below 0.5 so generation remains
  comfortably ahead of playback.
- Stop/skip targets audible playback termination within one second. Initial
  Kokoro soft cancellation may let one synchronous sentence finish silently.
  The single-run corpus median was 1.73 seconds, p90 was 2.67 seconds, and the
  maximum was 3.56 seconds; these are observations, not a guaranteed ceiling.
  User-visible skip latency also includes the next utterance's first-audio time.
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

Listening result on 2026-08-22: both the shared-model Piper set and Kokoro were
acceptable; the user preferred Kokoro. Advance Kokoro through the remaining
live-player, contention, cancellation, and runtime audit gates first. Retain the
measured Piper configuration as the lower-CPU fallback if Kokoro fails one of
those gates or its impact during real multi-agent work is unacceptable.

Runtime syscall tracing on 2026-08-22 observed no `socket`, `connect`, `sendto`,
or `recvfrom` calls from either installed engine during synthesis. Kokoro also
passed the complete corpus with its network namespace isolated. Under four
synthetic competing CPU workers, Kokoro's conversational case moved from its
uncontended baseline to 2.62-second first audio and 0.54 RTF at normal priority.
At niceness 5 it produced first audio in 3.23 seconds and 0.81 RTF; niceness 10
was similar at 3.36 seconds and 0.84 RTF. The test implementation defaults to
niceness 5 so agents/builds win CPU contention while synthesis remains ahead of
playback, but real multi-agent use still needs confirmation.

The embedded production worker, pinned user-local runtime, and real `mpv` path
also passed an end-to-end speaker test. From `go run`, cold time-to-first-sound
was about two seconds; inline `git status` remained audible and the transition
between two sentence segments had no click or awkward gap. The user accepted
that cold-start behavior. A later live multi-agent trial exposed occasional
start/finish clicks and gaps while a following sentence was synthesized. The
PCM edges were already near zero, ruling out an ordinary waveform-edge click;
the production path now retains one `mpv` process across normal utterances and
builds a four-second cancelable reserve in Go before playback. This avoids
routine audio-device reopen transients and synthesis underruns without placing
an unflushable multi-second cache inside `mpv`. A subsequent two-response test
coincided with a WSLg PulseAudio wedge: both responses were synthesized, the
player stopped, and `pactl` calls timed out. The empty persistent raw stream was
a plausible contributor, but causation was not established. Under WSL the
player now feeds zero-valued PCM only during a 30-second idle grace period, with
a bounded write deadline, then closes cleanly. This covers nearby responses
without maintaining indefinite background audio transport. After restarting
WSLg, both the direct production smoke command and a completed in-room Codex
response were audible while `pactl info` remained healthy. A following
two-agent trial played Codex and Claude in FIFO order without overlap, but the
final word of the first response arrived late and the standalone replay was cut
short. Capturing the exact production PCM stream showed every sentence segment
exactly once, ruling out an upstream duplicate. An `mpv` IPC probe instead
showed that an open raw stream retained roughly its final 0.4--0.5 seconds until
more PCM or EOF arrived. Repeated WSLg probes observed 0.58--0.64 seconds of
retained tail, so the WSL path now appends one second of zero-valued PCM
after each utterance and waits for `mpv`'s device-adjusted audio position to
cross the real speech boundary. An initial implementation incorrectly waited
for the end of the drain, which is unreachable while the raw stream retains its
open tail; the live two-voice test exposed that mistake as a completion timeout.
Immediate provider shutdown separately allows the already-written drain to
settle before closing the player. The exact phrase that previously truncated
was replayed in full and accepted by the user. The same failed live trial also
showed that treating an IPC confirmation timeout as device unavailability
incorrectly latched speech off before the next agent's turn. Completion
confirmation now falls back to a conservative cancelable duration estimate
when `mpv` is still running; only a stopped player or an audio write/device
failure latches speech unavailable. After rebuilding, the same two-agent prompt
played both complete voices in FIFO order with clean seams, no timeout, and no
unavailable latch; the user accepted that retry. The subsequent `/speak stop`
check halted a twelve-sentence response immediately; the user described it as
"super fast." The following two-agent `/speak skip` check yielded the first
voice promptly, played the second response cleanly, and left speech available;
the user accepted the result.

The same live trial confirmed four distinct configured voices and successful
speech for Codex, Claude, and Copilot. AGY received direct messages but AGY CLI
1.1.18 repeatedly failed to discover a correctly placed workspace custom agent;
its init event misleadingly echoed the requested agent name after falling back.
This matches the upstream print-mode issue. MoHuddle now uses a direct,
non-persistent AGY voice session in an empty temporary workspace with slash
expansion disabled, the native sandbox enabled, no auto-approved permissions,
and fail-closed handling for every emitted tool event. A live direct turn then
showed a second failure mode: after receiving the entire long-lived room
transcript, AGY completed successfully with only the private control marker and
therefore produced no public speech. AGY voice prompts are now limited to the
recent transcript tail; a directly addressed marker-only result gets one
smaller, explicitly focused retry, and a second empty result is surfaced as an
error rather than silently accepted. Optional review turns retain marker-only
silence without being forced into a filler response. After rebuilding and
restarting WSLg, a live direct `@agy introduce yourself briefly` turn produced a
normal public response and the user confirmed the response path was working.
Audible AGY output remains covered by the final playback acceptance pass rather
than being inferred from that text confirmation.

The live session later exposed a machine-level WSLg outage: `pactl info` also
returned `Connection refused`, confirming that synthesis was healthy but no
process could reach PulseAudio. `mpv` then fell through Pulse, JACK, and ALSA and
MoHuddle repeated the full diagnostic for queued responses. The production path
now selects Pulse explicitly under WSL, condenses that failure to one actionable
message, clears the queued speech, and pauses further playback attempts until
speech is re-enabled after WSLg recovers. This preserves text chat and prevents
error spam; it cannot repair a stopped WSLg server from inside the distribution.

Inline-code normalization was rechecked after the live gaps: text inside
backticks is retained and only the delimiters are removed. Empty quote/backtick
pairs are silent punctuation by design. Short controlled Kokoro samples did not
show a material silence increase for `--cache=no`; a symbol-heavy file path
produced about 0.36 seconds of natural pause, not the longer live dropout.

#### Decision gate

- [x] Record the spike results in the repository and select one of: Kokoro local,
  Piper local, neither local candidate, or more investigation required.
- [x] Do not select solely from published quality claims or repository popularity.
  Use the WSL measurements, listening results, dependency audit, and maintenance
  risk together.
- [ ] If neither local candidate passes, decide explicitly whether to retain the
  current non-streaming Edge path temporarily, implement opt-in Edge streaming,
  evaluate paid OpenAI TTS, or leave speech unavailable. Do not silently convert
  a failed local selection into cloud egress.

#### Phase 1: integrate the selected local provider

- [x] Introduce the synthesizer/audio-player boundaries and migrate the current
  Edge-specific provider without changing queue or UI behavior.
- [x] Add provider selection and provider-specific paths/model settings with a
  settings-version migration. Preserve existing voice mappings where names are
  still valid; otherwise report that remapping is required.
- [x] Start, validate, warm, monitor, and stop the selected local worker without
  blocking MoHuddle. Surface concise unavailability/failure diagnostics.
- [x] Feed sentence-level audio through a cancelable Go-side reserve into one
  persistent player so speech begins before the full response is synthesized,
  remains continuous across segment boundaries, and does not reopen the audio
  device for every response. Bound producer lead to two synthesized segments and
  use `mpv` IPC to observe the rendered speech boundary so queue ownership does
  not advance early.
  Under WSL, feed bounded zero-valued PCM during a 30-second idle grace period,
  then close the player so nearby responses can share the device without
  indefinite background audio transport.
- [ ] Carry timing diagnostics: T0 enqueue/provider request, T1 first audio from
  synthesizer, T2 first player write, T2b player-start/audio-output-ready proxy,
  T3 synthesis complete, and T4 playback complete. Keep routine UI output quiet.
- [x] Correct normalization and add regression cases for inline backticks,
  fenced code, Markdown, links, tables, JSON, terminal output, stack traces, and
  mixed prose.
- [x] Add deterministic service/provider/player tests for FIFO non-overlap,
  voice selection, continuous multi-segment playback, failure isolation,
  stop/skip, shutdown, and process cleanup. Keep a manual WSL audio checklist.
- [x] Update README installation, configuration, privacy, license, model-source,
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

- [x] A reviewed local provider runs without network access after installation,
  passes the agreed latency/quality gates, and supports stable distinct agent
  voices.
- [x] Audible speech begins before the complete response is synthesized, using
  one persistent player across normal utterances with no audible segment or
  response-boundary seams.
- [x] Multiple completed responses remain FIFO and never overlap.
- [x] Inline backticked text is spoken; full technical blocks are not read aloud;
  the stored/displayed response remains byte-for-byte unchanged.
- [x] Stop and skip halt playback within one second, discard any in-flight
  synthesis result, start no later segment, and preserve correct queue
  semantics without orphaning a worker or player.
- [x] Missing dependencies, invalid voices, provider errors, and audio failures
  are nonfatal. Text chat continues; an unavailable audio device clears queued
  speech and pauses playback retries until the user re-enables it after recovery.
- [x] Existing speech controls and persistence remain compatible, and operation
  with speech disabled is unchanged.
- [x] No cloud provider receives response text unless the user explicitly selects
  it, and local failure never enables cloud fallback.
- [x] No runtime/model dependency is bundled or redistributed without a recorded
  source, checksum strategy, license review, and maintenance decision.

### Optional participants

- [x] Keep AGY and Copilot isolated and tool-free by default while allowing an
  explicit workspace or full permission profile to enable coding tools, saved
  native sessions, attachments, and filesystem grants for direct turns.
