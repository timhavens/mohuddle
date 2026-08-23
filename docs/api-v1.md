# MoHuddle local API v1

MoHuddle exposes one transport-neutral command-and-event protocol. The first
transport is a local Unix socket; TCP, HTTP, WebSocket, automatic LAN discovery,
and unauthenticated listeners are not implemented.

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
| `room.get` | `observe` | none | sanitized room state |
| `history.get` | `observe` | optional `after`, `limit` (maximum 1000) | ordered messages and `has_more` |
| `status.get` | `observe` | none | room, active cores, availability, and correction statistics |
| `message.send` | `participate` | `mode` (`post`, `ask`, `round`) and `text` | accepted message ID |
| `command.invoke` | varies | `command`, optional `participant` | acceptance |
| `events.subscribe` | `observe` | none | acknowledgement followed by events |

The exposed v1 commands are `continue`, `stop`, `join`, and `leave`. Roster
changes require `administer`; the other two require `participate`. Provider
settings, grants, permission elevation, room switching, and full-access
acknowledgement are deliberately not exposed.

Peer and bridge credentials are restricted guests even if incorrectly assigned
broader scopes: they may send only `ask` messages, which use MoHuddle's isolated
read-only turn contract, and cannot invoke room-control commands. Creating and
exchanging peer credentials is not automated yet.

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
public messages, agent deltas, tool-safe activity, routing, turn/wave lifecycle,
warnings, conflicts, errors, and round completion. Attachment metadata omits host
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
