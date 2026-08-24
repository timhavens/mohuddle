# TODO

This file tracks actionable work. Completed implementation history remains in
Git; durable behavior, security boundaries, and operating instructions belong
in `README.md` and `docs/`.

## Requested features

### Concurrent room conversations and work routing

This is the highest-priority milestone. Preserve this checklist until every
acceptance requirement is verified. The user must be able to speak naturally
in the room at any time without tags or memorized commands, while writable work
continues safely in a separate single-writer workflow.

#### Durable input and consistent routing

- [ ] Persist and display every accepted room input before routing it. Every
  input must reach a visible terminal outcome: chat answered, planning or
  implementation started/queued, needs routing, timed out, unavailable,
  failed, or cancelled. Never silently lose an input or imply that work was
  implemented when nothing changed.
- [ ] Route by message meaning rather than transient room activity. The same
  text must route consistently while idle, busy, or between turns, and local
  routing must complete within two seconds without lead bidding.
- [ ] Treat questions, explanations, status requests, and ordinary discussion
  as independent read-only conversations whether main work is idle or active.
  Label their result `Answered as chat — no work was implemented`, with visible
  actions to add or queue the conversation as work or cancel it.
- [ ] Treat requests to implement, change, fix, build, run, commit, push, or
  otherwise mutate state as work directives whether main work is idle or
  active. Start them when idle or queue them durably behind active work; never
  hand them to a read-only responder as if implementation occurred.
- [ ] Preserve the workflow mode stamped at acceptance. Plan-mode work remains
  read-only and requires explicit approval; Default-mode work may implement.
- [ ] Prefer ambiguity over a silent wrong action. Uncertain inputs must show an
  inline `Answer as chat` / `Add to work` / `Replace current work` / `Cancel`
  choice rather than guessing. Replacing work requires confirmation.
- [ ] Require read-only responders to flag mutation requests back to `Needs
  routing`. Display route state and whether files or external state could have
  changed on every input and result.

#### Independent conversation scheduling

- [ ] Add durable conversation jobs and assign them without bidding: idle
  auxiliary/optional participant first, then an idle core not immediately
  needed by main work, then a provider-aware temporary read-only responder,
  then a persistent queue with visible position.
- [ ] Keep main-work turns at higher scheduling priority. Temporary responders
  must respect provider availability, cooldowns, quotas, and saturation and
  must not reduce the main workflow's usable provider capacity.
- [ ] Default to at most two simultaneous temporary responders, configurable
  from zero to eight, and one temporary responder per provider. Bind each
  temporary responder to one conversation so unrelated context cannot leak.
- [ ] Give each conversation a visible adaptive total budget shared by all
  attempts: Quick 20 seconds with retry after 10, Standard 60 seconds with
  retry after 30, Research 120 seconds with retry after 60. Allow one retry on
  a different available provider.
- [ ] Show responder, elapsed time, deadline, retry state, and queue position.
  At total-budget expiry move to `Needs attention` with Retry, Keep waiting,
  and Cancel; never leave a job indefinitely Finding, Waiting, or Answering.

#### Context isolation and durable recovery

- [ ] Give every conversation an internal ID that users never manage. Keep its
  messages visible in the room, but exclude unrelated conversation traffic
  from main-work prompts. Supply responders only the source question, linked
  follow-ups/corrections, current room status, and bounded relevant context.
- [ ] When a conversation is promoted to work, explicitly link only its
  relevant messages into the main workflow. Preserve the single-writer rule:
  multiple work directives may queue, but only one writable workflow runs.
- [ ] Persist classification/confidence, stamped mode, state, queue position,
  response class/deadline, responder/provider, attempts, linked answer,
  unread state, lifecycle timestamps, terminal reason, and remote message ID.
- [ ] On restart restore waiting jobs, requeue interrupted attempts within
  their remaining budget, convert expired attempts to Needs attention, reap
  expired temporary responders, deduplicate remote delivery, and publish only
  the first valid answer when attempts race.

#### Replies, follow-ups, and automatic retirement

- [ ] Pin the oldest unread answer above the composer until acknowledged. The
  pin must distinguish chat-only answers from completed work. Add a persistent
  Replies indicator, notifications, and jump-to-answer behavior with equivalent
  TUI and phone PWA interaction.
- [ ] After a temporary responder answers, keep it available for linked
  follow-ups or material corrections during a five-minute quiet grace period;
  linked activity resets the timer and the responder stays read-only.
- [ ] After five quiet minutes, close the temporary provider session/process,
  release its concurrency slot, and remove its live identity while preserving
  messages, answer links, corrections, and audit history. Retire failed or
  cancelled responders after persisting their terminal state. A later follow-up
  receives a new responder with the preserved bounded conversation context.

#### Permissions, remote controls, and cancellation

- [ ] Enforce read-only conversation responders regardless of provider defaults:
  no file edits, plan execution/approval, mode/roster/settings changes, access
  expansion, main-work steering, or external mutation.
- [ ] Observe devices cannot send; participate devices may ask read-only
  questions; trusted phone-admin devices may promote or replace work with the
  same confirmations as the TUI. Preserve authentication, CSRF protection,
  deduplication, rate limits, permission ceilings, and auditing.
- [ ] Individual Cancel affects one conversation. `Esc`, `/stop`, and the phone
  Stop button cancel active/queued work, conversations, and temporary responders.

#### Acceptance and delivery

- [ ] Test identical routing across idle/busy boundaries, bare imperatives,
  idle-room questions, Plan-mode enforcement, ambiguous choices, and explicit
  chat/planning/queued/completed labels.
- [ ] Test concurrent main work with multiple phone/TUI questions, two-second
  routing, all adaptive deadlines, retry/outage behavior, provider saturation,
  temporary responder limits, main-work priority, and multiple queues.
- [ ] Test restart in every lifecycle state, duplicate delivery, racing
  attempts, strict context isolation, work promotion/replacement, pinned
  replies, notifications, individual/global cancellation, grace reset,
  automatic retirement, and late follow-up after retirement.
- [ ] Run full tests, race/stress tests, vet, JavaScript validation, Windows
  cross-compilation, and CI. Update this checklist as work lands; commit and
  push only verified changes.

### Local API and MoHuddle federation

- [ ] Add the Windows named-pipe transport. It must use an explicit restrictive
  current-user DACL and preserve the Unix transport's authentication, framing,
  identity, lifecycle, and audit guarantees. Any loopback HTTP/WebSocket adapter
  must also authenticate clients. See `docs/api-v1.md`.
- [ ] Support hosted services such as ChatGPT through an explicit local bridge,
  connector, or outbound relay; do not assume a hosted client can contact
  localhost directly. Select a real service contract, authentication model,
  reconnect behavior, and permission ceiling before implementation.

## Future considerations

### Speech diagnostics

- [ ] Add quiet end-to-end timing instrumentation for both provider evaluation
  and production diagnosis: T0 enqueue/provider request, T1 first synthesized
  audio, T2 first player write, T2b player-ready proxy, T3 synthesis complete,
  T4 playback complete, plus cold worker-ready time, real-time factor, CPU/peak
  RSS, voice-switch overhead, long-response continuity, and stop/skip latency.
  Keep routine UI output quiet and expose measurements only through an explicit
  diagnostic path.

### Deferred optional cloud speech

- [ ] If Edge remains desirable, revive streaming as a separate opt-in adapter:
  one realistic long-utterance test first, then streamed playback without a
  temporary MP3, explicit network-failure handling, and measured first sound.
- [ ] If OpenAI TTS is desired, design it as a separately billed opt-in provider
  with explicit credentials and data-egress configuration. Do not present it as
  part of a Codex or ChatGPT subscription.

Cloud speech is never an automatic fallback. If local speech is unavailable,
speech stays unavailable and text chat continues; this policy is documented in
`README.md`.
