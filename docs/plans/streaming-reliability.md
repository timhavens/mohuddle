# Streaming reliability and failed-reply recovery

The September 21, 2026 investigation of room `805288fe` found 19 retained
`codex event queue overflow` failures, including all five reported failed
draft replies. The Codex adapter rejected bursts when its 256-entry queue
filled. Every text delta also rebuilt the room transcript; an isolated test
using approximately 4,400 messages measured 72–75 ms per delta.

## Implemented design

- Route RPC responses directly to callers. Queue consumed Codex notifications
  in order, merging adjacent text deltas only within the same thread, turn,
  and item. Bound pending data to 4,096 entries and 16 MiB. A capacity failure
  terminates that transport with a typed local error rather than silently
  losing lifecycle events or repeating accepted work.
- Capture drafts independently of display. Store bounded latest previews
  outside the blocking room event channel and refresh the UI at most every
  50 ms. Lifecycle events remain ordered; finished previews cannot reopen.
- Cache transcript entries and invalidate changed content or presentation.
  Stream and activity events do not rebuild the transcript.
- Record host turn IDs on conversation attempts and whether draft capture
  was truncated. Expose the safe `event_queue_overflow` reason without marking
  the provider unavailable.
- Offer `mohuddle_read_reply_draft` for retained public drafts linked to failed
  ChatGPT replies. Require a current participation lease and a delivered source
  message. Pagination preserves Unicode and segment boundaries. Recovery
  spends no exchange and never publishes an answer. Legacy records require
  unambiguous recorded links; completeness may be unknown.

## Validation and rollout

Regression coverage includes bounded queues, burst delivery, slow consumers,
RPC delivery, stale turn filtering, concurrent previews, 5,000-message rooms,
cache invalidation, capture limits, recovery authorization, legacy links,
Unicode pagination, and panel error presentation. Run `make check` and
`node --test internal/chatgpt/panel_test.mjs` before release.

Release through the repository's CI and Release Please workflow. Verify the
published archive and local executable correspond to the release tag's commit.
Keep the active room untouched until an idle restart boundary; back up its
state before restart. Refresh ChatGPT's saved tool metadata for the eighth tool.
Effort management, workspace write serialization, and task authority are
separate concerns and are unchanged by this fix.
