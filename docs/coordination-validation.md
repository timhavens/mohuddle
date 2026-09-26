# Continuous coordination validation

Implementation and local validation: September 26, 2026. The running room and its
business workload were not changed. No release was installed or deployed.

## Implemented behavior

- One versioned source supplies the coordinator skill, initialization/join
  instructions, reminders on reads and notifications, and participant guidance.
- Monitored runs retain objectives and per-result handoffs independently of the
  recent event window. A notification or acknowledgment does not resolve a
  promised dispatch. Accepted linked assignments and explicit scoped reports do.
- Ready handoffs remain visible during independent work. Claims serialize panel
  notifications, with three automatic attempts at 0/60/180 seconds and a shared
  20-second spacing. Host rejection suppresses retries; unknown delivery remains
  unknown. Exhausted recovery stays visible.
- One explicitly registered, budgeted read-only review can follow successful
  writer completion without waiting for another ChatGPT turn. It uses the normal
  scheduler, the actual retained result, an exact reviewer and permissions.
- Explicit pauses, stop, leave, grant expiry/revocation and restart are respected.
  Passive participation expiry alone does not cancel the registered review.
  Restart reconciles saved dispatches; it cancels unstarted reviews.

## Local evidence

| Check | Result |
| --- | --- |
| All `internal/chat` and `internal/room` tests | Passed, including normal scheduler behavior. |
| New direct API tests | Passed: guidance delivery, reconnect recovery, linked/idempotent dispatch, two-exchange reservation, continuation receipt and competing notification claims. |
| In-memory MCP initialization/schema test | Passed: shared instructions and additive tool inputs are exposed. |
| JavaScript panel tests | Passed: 24 cases, including legacy behavior, independent handoffs, accepted-but-unresolved retry, unknown delivery, competing claims, hidden panel and new human direction. |
| Focused race tests across chat/room/API/bridge | Passed. |
| `go vet ./...` | Passed. |
| Candidate build and `--version` smoke test | Passed using a separate `/tmp` executable. |
| Skill frontmatter/reference validation | Passed; an independent, offline behavioral evaluation also informed the guidance. |
| `make check` and full race suite | Attempted; tests requiring Unix/TCP listeners cannot run in this sandbox (`operation not permitted`). |
| Vulnerability and secret scanners | Attempted separately; their pinned tools cannot fetch Go module metadata because network/DNS sockets are denied. |
| Actual ChatGPT website trials | Not performed. No claim of website delivery or unattended turn reliability. |

The new lifecycle cases include a synthetic 38-minute handoff gap, event-history
eviction, persistence/restart, scoped blockers, clearing a global blocker,
resumption of a failed workflow, passive lease expiry, lost dispatch receipts,
and missing/oversized results. These are deterministic tests with test agents,
not provider or website trial results.

Repeat the checks from the repository root:

```sh
go test ./internal/chat ./internal/room -count=1
go test ./internal/api ./internal/chatgpt -run 'TestHandoff|TestCoordinationGuidance|TestLivePanel' -count=1
go test -race ./internal/chat ./internal/room ./internal/api ./internal/chatgpt -run 'TestHandoff|TestContinuation|TestRegisteredContinuation|TestCoordination|TestLivePanel' -count=1
node internal/chatgpt/panel_test.mjs
go vet ./...
make check
```

## Live validation before relying on unattended coordination

Use a separate authorized test room and disposable workspace. After full checks
pass in an unrestricted development environment, install the candidate at an idle
point, restart its host/bridge, refresh ChatGPT's saved tool metadata, rejoin, and
verify `instruction_version`, advertised capabilities and the panel resource.
Keep the existing business room and stopped objectives unchanged.

Run at least five matched short writer/reviewer tasks on the baseline and the
candidate using the same model, effort, scope and limits. Record the actual
result-ready, notification claim/outcome, explicit acknowledgment and accepted
next-assignment timestamps, along with whether a human had to intervene. Check
both manual ChatGPT dispatch and the explicitly registered review path.

Exercise these situations separately:

1. The writer finishes while a different participant is still busy. Only the
   applicable dependency should delay the next step.
2. The user asks for status during the task. ChatGPT should answer and continue
   the same authorized objective.
3. Website delivery is accepted but ChatGPT does not dispatch. The handoff must
   stay open, retries remain bounded and exhaustion becomes visible.
4. The panel is hidden, closed, unsupported or disconnected. Availability must
   become explicit or unknown after 45 seconds; do not infer a delivered turn.
5. A reply fails or a dispatch receipt is lost. Recover retained evidence and
   inspect the exact saved operation before retrying; no duplicate assignment.
6. Pause, stop, leave, grant expiry and host restart occur before the review.
   No unstarted continuation may escape these boundaries or revive on renewal.

Compare median and slowest result-to-assignment delays and count manual
interventions and duplicates. Retain individual timing evidence rather than
claiming success from averages alone. A result waiting over three minutes must
be visible with an owner, next action or blocker and delivery status. Accepted
notification delivery is not evidence that ChatGPT acted.

The metrics in `coordination.metrics` are counts and cumulative wall-clock
handoff latencies. `stalled_seconds` totals elapsed handoff time beyond the
three-minute threshold; it is not measured model inactivity or productive work
time. Review legitimate waits and external blockers separately in comparisons.
No percent-complete estimate is inferred.

If a regression appears, disable live follow-ups and omit new continuation
registrations while investigating. Stop the monitored run to cancel queued
continuations; use local `/stop` to cancel running work. Reverting an executable
requires an idle restart and does not itself establish that active jobs stopped.
