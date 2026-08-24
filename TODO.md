# TODO

This file tracks actionable work. Completed implementation history remains in
Git; durable behavior, security boundaries, and operating instructions belong
in `README.md` and `docs/`.

## Requested features

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
