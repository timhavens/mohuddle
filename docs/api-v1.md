# MoHuddle local API v1

MoHuddle exposes one transport-neutral command-and-event protocol. Local clients
use a private Unix socket. Explicitly paired instances may use TLS 1.3 over TCP
with pinned instance certificates. An explicitly enabled same-origin HTTP and
WebSocket gateway serves paired phone devices. Automatic LAN discovery and
unauthenticated listeners are not implemented.

The wire format is newline-delimited JSON. Every request and response carries
`"version":"mohuddle.v1"` and a caller-provided request `id`. A connection must
authenticate with `hello`, then bind its immutable identity to the current room
with `room.join`. Event subscriptions use a separate connection because a
successful `events.subscribe` response changes that connection into an
event-only stream.

## Local endpoint and credentials

On Linux and other Unix platforms, the default socket is:

```text
$XDG_STATE_HOME/mohuddle/api-ROOM_ID.sock
```

It falls back to `$HOME/.local/state/mohuddle`. The socket is mode `0600` inside
the mode-`0700` state directory. `--api-socket PATH` selects another socket, but
its parent must already be private (`0700`); MoHuddle refuses shared parents and
never changes their permissions. `--no-api` disables the endpoint.

The first launch creates `api_credentials.json` with mode `0600`. Its initial
`local-admin` credential has `observe`, `participate`, and `administer` scopes.
Treat its token like a password. Each successful connection receives an
immutable namespaced identity:

```text
INSTANCE_ID/CREDENTIAL_ID/CLIENT_ID
```

Connections, authentication attempts, and requests are appended to
`api_audit.jsonl`. Neither room views nor history expose workspace paths,
filesystem grants, native session IDs, agent settings, or attachment host paths.

Windows named-pipe support is not implemented yet; the endpoint remains disabled
there unless and until an OS-protected named-pipe transport is added.

## Paired phone gateway

The embedded phone PWA and browser gateway are disabled unless the host supplies
`--remote-listen HOST:PORT`. Cleartext HTTP is accepted only on a loopback bind
for use behind an encrypted authenticated tunnel. A non-loopback bind requires
`--remote-tls-cert`, `--remote-tls-key`, and an exact HTTPS
`--remote-origin`; every API and WebSocket request must match both that Origin
and Host. The gateway also applies strict CSP, frame, referrer, MIME, and browser
Permissions-Policy headers.

For TLS termination in a tunnel or reverse proxy, bind MoHuddle on loopback and
set `--remote-origin` to the public HTTPS origin. The proxy must preserve its
public Host header. The HTTPS origin makes the session cookie Secure even though
the private proxy-to-MoHuddle hop uses HTTP.

Trusted local TUI commands create and manage room-bound device grants:

```text
/remote pair observe|participate DEVICE_NAME
/remote devices
/remote scope DEVICE_ID observe|participate
/remote revoke DEVICE_ID
/remote audit
```

An invitation is single-use, expires after 15 minutes, and is stored only as a
hash. `POST /api/v1/pair` consumes it with a browser-generated P-256 SPKI public
key. `POST /api/v1/challenge` returns a short-lived plaintext challenge payload;
the browser signs its UTF-8 bytes with WebCrypto ECDSA P-256/SHA-256.
`POST /api/v1/session` verifies the raw 64-byte signature and creates an
independently expiring in-memory session with an HttpOnly, SameSite=Strict
cookie and a CSRF token.

Authenticated `POST /api/v1/request` accepts `room.join`, `room.get`,
`history.get`, `status.get`, and `message.send`. The gateway supplies the
authenticated route; browser-provided route data is discarded. Observe devices
cannot send. Participate devices may send only `mode:"ask"`, and the bridge
session fixes the triggered agent execution ceiling at read-only. Admin/device
scope elevation is intentionally not exposed by this first slice.

`GET /api/v1/events` upgrades to WebSocket and accepts `room_id`, `boot_id`,
`after_event`, and `after_message` query values. The first frame is:

```json
{
  "type": "sync",
  "cursor": {"boot_id":"...","event_sequence":42,"message_sequence":900},
  "history": {"messages":[],"has_more":false,"next_after":900,"through":900,"latest_sequence":900},
  "room": {"id":"ROOM"}
}
```

Later frames have `type:"event"` and one stable cursor, or `type:"gap"` with
`boot_mismatch`, `cursor_expired`, `subscriber_overflow`, or
`upstream_overflow`, the current replay cut, and `history_after`. The event
journal is process-local and bounded by record count and encoded bytes. The sync
frame contains at most one 100-message stable-through history page, and its event
cursor acknowledges only replay frames already delivered. The transcript
remains durable: the client pages `history.get` through the captured message
high-water after a restart or gap.
Revoking a grant immediately invalidates its sessions and closes active streams.
Changing a grant between observe and participate also invalidates every existing
challenge/session so the next proof receives exactly the new host-selected scope.

Remote audit records include device/session/room identity, scopes, read-only
ceiling, action, allow/deny result, and errors, but never codes, keys, signatures,
cookies, CSRF values, or request payloads.

## Explicit instance pairing

> This is an experimental API-federation layer. It authenticates one instance
> to another and exposes the restricted v1 operations described below. It does
> not automatically mirror TUI rooms or add remote agents to a room roster.

Federation is disabled unless the host supplies `--federation-listen HOST:PORT`.
MoHuddle never broadcasts, discovers, or joins peers automatically. Each state
directory has a persistent ECDSA instance key in `federation_identity.json` and
directional grants in `federation_pairings.json`; both files are mode `0600`.

Create a short-lived invitation on the hosting instance:

```bash
mohuddle pair invite --address HOST:7443 --ttl 15m > pair.invite
mohuddle --federation-listen 0.0.0.0:7443
```

The invitation contains the advertised address, immutable host instance ID,
host certificate fingerprint, expiry, and a random one-time secret. The host
stores only the secret hash. Transfer the invitation through a channel you trust
and accept it on the other instance through stdin:

```bash
mohuddle pair accept < pair.invite
```

Acceptance connects with TLS 1.3, pins the exact host certificate from the
invitation, presents the accepting instance's certificate, consumes the
one-time secret, and returns a long-term random peer token inside that encrypted
connection. Subsequent connections require both the token and possession of the
pinned private key. Pairing is directional; repeat the process in reverse when
both instances must initiate connections.

Useful management commands are:

```bash
mohuddle pair list
mohuddle pair check --peer HOST_INSTANCE_ID --room REMOTE_ROOM_ID
mohuddle pair revoke INSTANCE_ID
```

Listings never print tokens, invitation hashes, or private keys. Revocation is
reloaded by a running listener, rejects new handshakes, and closes active event
streams. A lost pairing response cannot reuse the consumed invitation; create a
new invitation. There is no certificate-authority trust fallback: a changed
certificate requires intentional re-pairing.

The listener may be exposed on a private network or through an authenticated
tunnel, but binding it beyond localhost is an explicit operator action. MoHuddle
does not configure firewalls or claim that public-Internet exposure is safe.

## Handshake and room binding

The token appears only in the initial request:

```json
{"version":"mohuddle.v1","id":"hello-1","type":"hello","payload":{"client_id":"terminal","token":"TOKEN"}}
```

A successful response returns the assigned identity, instance identity, client
kind, and scopes. Bind the connection to the running room next:

```json
{"version":"mohuddle.v1","id":"join-1","type":"room.join","payload":{"room_id":"ROOM_ID"}}
```

One endpoint hosts one active room. Binding a client does not add or remove an AI
participant; the administrative `join` and `leave` commands control that roster.

## Requests

| Type | Scope | Payload | Result |
|---|---|---|---|
| `room.join` | `observe` | `room_id` | bound room ID |
| `room.get` | `observe` | none | sanitized room state, including `workflow_mode`, scheduled roster-action audit records, and pending human-input count |
| `history.get` | `observe` | optional `after`, stable `through`, `limit` (maximum 1000) | ordered messages, `has_more`, `next_after`, and latest sequence |
| `status.get` | `observe` | none | room, active cores, availability, and correction statistics |
| `message.send` | `participate` | `mode` (`post`, `ask`, `round`) and `text` | accepted message ID; `post` queues at the next safe boundary when work is active |
| `command.invoke` | varies | `command`; optional `participant`, `action`, `execute_at`, `reason`, `action_id` | acceptance |
| `events.subscribe` | `observe` | none | acknowledgement followed by events |

The exposed v1 commands are `continue`, `stop`, `join`, `leave`,
`roster.schedule`, and `roster.cancel`. Immediate and scheduled roster changes
require `administer`; `continue` and `stop` require `participate`. A scheduled
action uses `action` (`join` or `leave`), `participant`, a future RFC3339
`execute_at`, and optional `reason`; cancellation uses `action_id`. Only one
pending action per participant is accepted. These records survive restart,
execute only at idle workflow boundaries, and remain in `room.get` after
execution, cancellation, or failure. Provider settings, grants, permission
elevation, room switching, and full-access acknowledgement are deliberately not
exposed.

Peer and bridge credentials are restricted guests even if incorrectly assigned
broader scopes: they may send only `ask` messages, which use MoHuddle's isolated
read-only turn contract, and cannot invoke room-control commands.

Normal `post` input never cancels an active workflow. It is persisted as pending
input, omitted from the running agents' prompts, and dispatched after the room
reaches an idle boundary. Local TUI `/steer` is intentionally not exposed as a
v1 remote command; remote clients must use `stop` and then `post` when their
authorization permits an explicit replacement.

The trusted local TUI controls the room's `execute|plan` workflow mode. Each
accepted human transcript message exposes its stamped `workflow_mode`, and
turn-start events expose the mode being enforced. A mode change affects only
future submissions: queued messages retain their stamped mode across restart.
Plan workflows are host-enforced read-only and never execute automatically.
Remote clients may observe the mode and submit into it, but v1 deliberately
does not expose a command that changes it.

## Routing and replay protection

Every mutating request requires an authenticated route:

```json
{
  "message_id": "GLOBALLY_UNIQUE_ID",
  "origin_instance_id": "INSTANCE_ID",
  "origin_client_id": "ASSIGNED_CLIENT_IDENTITY",
  "hops": ["PRIOR_INSTANCE_ID"]
}
```

Local origins must match both identities assigned during `hello`; clients cannot
impersonate another origin. MoHuddle rejects malformed routes, duplicate message
IDs, a route that already contains the receiving instance, and routes at the
eight-hop bound. It appends its instance to accepted routes and persists the
metadata atomically with the public message. The bounded deduplication cache is
rebuilt from transcript history after restart. Message IDs are at-most-once
operation keys and must not be reused after any dispatch attempt, including one
that returns `command_failed`. Causal/vector ordering is not part of v1.

## Events

Events have their own globally unique IDs and route metadata. Payloads represent
public messages, agent deltas, tool-safe activity, routing, queue changes,
turn/wave lifecycle, warnings, conflicts, errors, and round completion. Local
turn-start payloads include the host-derived `role` and `task`; queue-change
payloads include `queued`. Attachment metadata omits host
paths. Peer and bridge streams do not receive host warning/error text or
tool-event text. A slow subscriber never blocks the room; if its bounded buffer
overflows, the next available event is an `event stream gap` warning and the
client must recover with `history.get`.

Errors use a stable shape:

```json
{"version":"mohuddle.v1","id":"request-id","ok":false,"error":{"code":"forbidden","message":"..."}}
```

Known codes include `unsupported_version`, `invalid_request`, `unauthenticated`,
`authentication_failed`, `forbidden`, `not_joined`, `room_not_found`,
`invalid_route`, `routing_loop`, `hop_limit`, `duplicate_message`, and
`command_failed`.
