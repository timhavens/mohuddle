# ChatGPT-managed effort per request

Implemented September 21, 2026. The [user guide](../chatgpt.md#effort-for-each-task) documents the tool fields and connection refresh procedure. Implementation and regression tests cover request persistence, concurrent choices, native default restoration, model revalidation, and connection guidance.

## Summary

Let ChatGPT select effort when requesting work, peer replies, or moderated rounds. Support Codex, Claude, AGY, and Copilot, including auxiliary participants.

Selections belong to the request. They do not change standing `/effort` settings or affect unrelated tasks.

This is the recommended first automation: ChatGPT already understands the task, so it can choose effort without an additional classifier call. Lower effort can reduce reasoning-token use, but savings must be evaluated alongside result quality. [OpenAI reasoning guidance](https://developers.openai.com/api/docs/guides/reasoning)

## Behavior and interfaces

- Add optional `effort` to `mohuddle_request_work`.
- Add optional `efforts`, keyed by participant, to `mohuddle_publish` with requested replies and `mohuddle_request_round`. Allow separate effort for the round moderator.
- Add optional `effort_reason` to these requests: a brief, shareable explanation of the selection.
- Omitted effort preserves existing room behavior. Do not treat omission as an economical default or reinterpret the existing `auto` setting.
- Expose participant effort capabilities in join/read/panel results: current model, standing effort, available levels, and whether capability information is model-specific or only provider-level. Include the current moderator.
- Return accepted effort selections in receipts and retain them in work/reply status. Show requested effort separately from provider-confirmed effort; unknown confirmation stays unknown.

Update ChatGPT’s embedded instructions, tool descriptions, and usage guidance:

| Task | Normal choice |
|---|---|
| Straightforward lookup, small mechanical edit, concise summary | `low` |
| Ordinary implementation, bounded debugging, routine review | `medium` |
| Difficult debugging, architectural reasoning, complex review | `high` |
| Higher supported levels | Only when explicitly requested by the user |

ChatGPT should explicitly select a supported level for each scheduled participant. This is behavioral guidance, not a hard spending limit enforced through a new approval flow.

## Implementation

- Pass typed effort selections through the existing MCP → private API → orchestrator path. Keep command text inert and preserve existing participation, permission, and budget checks.
- Persist selections with the originating message and workflow/conversation records. Apply them when building each relevant `TurnRequest`, including continuations and retries for that participant within the accepted operation.
- Keep selections participant-specific. Other participants retain their settings unless explicitly included; do not propagate one provider’s effort value into another provider.
- Validate before posting or scheduling. Reject invalid levels, unrelated participant keys, and effort on a text-only post. Use model catalog capabilities when available and existing provider validation otherwise, clearly labeling the latter’s uncertainty. Never silently replace a rejected selection with a more expensive one.
- Cache capability metadata by provider/model. Reads must remain available while agents are busy; unavailable discovery must not block room participation. Revalidate queued selections against the model used at dispatch and report incompatibility without silently dropping the override.
- Apply settings under the existing participant execution gate. Codex should retain its native session. For adapters that reset sessions when effort changes, detect the reset before preparing transcript context and replay the necessary history.
- Ensure the next unrelated task restores its effective room effort, including a provider-default baseline. Do not assume omitting an effort parameter clears a previous native-session setting.
- Include effort and its reason in operation-id equality checks. Identical retries reuse the original operation; changed selections require a new operation. Keep repetition protection insensitive to effort changes so changing levels cannot evade it.
- Add effort details to existing task/turn status and the ChatGPT panel. Update documentation with examples and the existing bridge/schema refresh procedure.

## Validation

- A `low` request runs at low while the room remains configured at `max`; the next unrelated task uses max.
- Concurrent and queued requests retain independent selections through completion, failure, cancellation, and supported restart recovery.
- Multi-peer replies and rounds apply distinct choices, including the moderator.
- Every provider receives the expected effort; auxiliary participants resolve their underlying provider correctly.
- Session resets preserve context, and returning to provider-default effort does not retain a previous override.
- Invalid choices fail before posting; changed effort conflicts with an existing operation ID; identical retries do not dispatch twice.
- Old requests and saved rooms without new fields retain existing behavior. Permissions, Plan mode, exchange limits, and repetition protections remain intact.
- Run `make check` and the panel’s Node tests. Manually compare a small edit and a difficult debugging task, recording effort, available usage data, and result quality without asserting a fixed savings percentage.

## Defaults and boundaries

No standing-setting control tool, model switching, room-wide classifier, automatic escalation/retry loop, or new billing dashboard in this release. ChatGPT reads actual results before deciding whether a separately authorized higher-effort follow-up is warranted.

## Live validation record

On September 21, 2026, an isolated room used authenticated Codex with `gpt-6-astra` while its standing effort remained `max`:

| Task | Requested/applied effort | Elapsed | Independently verified result |
|---|---|---|---|
| Change a Markdown title | `low` | 16.689 s | Exact requested title; body unchanged |
| Repair a concurrent cache | `high` | 52.032 s | Existing tests unchanged; independent `go test -race ./...` passed for same-key single-flight, unrelated-key concurrency, nested reads, and retry after errors |

The provider identified its model but did not confirm effort through the adapter, so both statuses correctly remained unconfirmed. Its native session records independently recorded `low` and `high`, respectively. The adapter does not expose token totals; inspecting only the usage counters in those isolated native session records gave:

| Task | Input tokens | Cached input tokens (included in input) | Output tokens | Reasoning output tokens (included in output) |
|---|---:|---:|---:|---:|
| Title edit, `low` | 37,705 | 18,560 | 257 | 0 |
| Cache repair, `high` | 123,658 | 99,968 | 944 | 35 |

These are cumulative native session counters, not billing totals. They establish working controls and acceptable results on these fixtures; they do not establish a savings percentage or compare equal tasks at different levels. Input/context costs remain relevant even when reasoning effort is low.

Repeat with `MOHUDDLE_LIVE_EFFORT=1 go test ./internal/integration -run TestLiveChatGPTEffort -v -count=1 -timeout 9m`. This opt-in test uses a disposable workspace and its own room, leaving running rooms untouched. Set `MOHUDDLE_EFFORT_MODEL` to test a particular supported model.
