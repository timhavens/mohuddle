# Coordination reliability and retained roadmap

Approved September 23, 2026. ChatGPT remains the coordinator. The first release
observes coordination and improves reply delivery; it does not dispatch future
workflow stages. The stopped Booking Platform backlog must remain stopped.

## First release

- Explicit local monitored runs, durable identity/state, ten-minute idle warning,
  and one-hour operation-completion diagnostic. Polling and acknowledgements are
  not progress; an operation completing does not prove a backlog item is vetted.
- Correlated result availability, panel notification attempt, reported website
  acceptance/rejection/unknown, explicit coordinator acknowledgement, and next
  accepted assignment. Missing evidence remains unknown.
- Quick replies retain ten-minute deadlines; explicit research replies receive
  thirty minutes. Effective class/deadline/effort remain visible and retries are
  idempotent only for the same request.
- Complete shared-message pagination with Unicode-safe offsets and version
  hashes; reuse interrupted-draft recovery, preserving incomplete/truncated and
  unavailable-retention distinctions. No arbitrary filesystem access.
- Stop survives grant renewal, polling, rejoining and restart. Only local
  monitoring controls can resume. Monitoring grants no new task authority.
- Deterministic lifecycle tests, real bridge/panel coverage, synthetic flow and
  failure demonstrations, repository checks, CI, release, and verification of the
  installed and running release executable at an idle point.

## Continuous-coordination implementation

The September 26 implementation adds centrally managed, versioned guidance,
durable result handoffs, per-branch reports, claimed notifications with bounded
recovery, and explicitly registered single read-only review continuations. See
[the operating documentation](../chatgpt.md#continuous-coordination-instructions-and-handoffs)
and [validation status](../coordination-validation.md). The historical first-release
scope above remains the record of that release. No existing business workload is
resumed by installing these changes.

## Remaining code work

1. **Broader local continuation:** consider multi-stage transitions only after
   validating the implemented single read-only review stage. No automatic writable
   stages or semantic approval of review prose are implemented.
2. **Incremental per-item output:** checkpoint completed units during research;
   failed final responses must not discard usable completed work.
3. **Task-specific effort profiles:** explicit investigation, review, mechanical
   verification and relay profiles without silent preference overrides.
4. **Claude sandbox repair:** diagnose `bwrap: Can't mkdir /etc/claude-code`, fix
   the supported configuration, and expose missing capabilities before dispatch.
5. **Structured item/review tracking:** authorship, reviewer eligibility, exact
   candidates, blockers, and stages; automate repetitive bookkeeping.
6. **Concurrent isolated draft writes:** separate authorized draft roots may run
   concurrently while canonical documents retain one writer.
7. **Throughput reporting:** distinguish execution, queueing, handoff delay,
   retries and verified outcomes; support measured before/after comparisons.
8. **Broader artifact delivery:** durable shared artifacts when message paging
   is insufficient, with explicit access and retention boundaries.

These are retained proposals, not implemented capabilities or a new authorization
to resume a stopped workload.

## Procedural recommendations retained for future review

- Separate approved wording from implementation readiness; unresolved decisions
  may remain explicit without preventing a clear task from being saved.
- Keep an independent reviewer out of correction authorship.
- Review material corrections proportionately and reuse unchanged evidence.
- Keep current status compact; avoid rebuilding large evidence packages for
  minor edits.
- Keep one or two groups ahead; park blocked items while independent work moves.
- Demonstrate two complete groups, including a timeout and a blocked decision,
  before relying on unattended operation.

These remain workflow choices, not additional gates imposed by MoHuddle.
