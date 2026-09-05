# Local agent access through the running Chess service

Status: candidate decision. Adoption and independent review are still required.
This note authorizes no transport implementation or public activation.

## The observed gap

At main `e7a65612`, `chess serve` owns the repository writer. The actual
own-agent diagnostic showed that CLI and MCP board reads work beside it, but
both move attempts refuse because another Chess writer owns the repository.
Stopping the service lets the same MCP move succeed; the service cannot restart
until that MCP writer exits. The diagnostic kept the OS lock intact and reaped
its processes. It is evidence of a transport gap, not browser acceptance.

A local game needs one continuously running service to accept both the human
browser's signed actions and the agent's signed actions. The agent should not
need repository writer access to join, answer moves, finish a game or arrange
a rematch.

## Decision

Add an explicit local service mode to the existing CLI and stdio MCP adapter.
The service remains the sole writer and preserves the current forge confirmation
rule. The agent keeps its own private key and signs locally. Server mode never
opens a local writer, even when the service is unavailable.

The minimum configuration is a loopback service URL, the expected canonical
repository genesis, and an explicit agent key file. For example, after the
operator starts the service:

```text
chess mcp --server http://127.0.0.1:8080 --genesis <canonical-genesis> --key <agent-key-file>
```

These are proposed flags, not commands available in the current release.
Reject an explicitly supplied local `--repo` together with `--server`; do not
silently choose one destination. Require a literal loopback address, HTTP,
no user information, query, fragment or path prefix, no redirects and no
ambient proxy. Public origins and remote agents remain outside this decision.
A small read-only service description supplies its canonical genesis, transport
version and supported operations. Compare the configured genesis before any
signature or submission; a reconnect repeats that check.

Provide explicit standalone agent-key creation using the existing secure key
custody helpers. It creates a new key only on request and never prints private
material. Server mode reads an existing key and refuses a missing, replaced or
unsafe key file. Losing a key is not permission to create another and claim its
seat. Existing identity anchoring and seat recovery rules remain unchanged.

The service is a trusted local intent encoder, as it is for the current browser.
Checking a returned echo or genesis is not independent authentication of a
malicious service. This mode is for an operator-selected process on the same
host, not a way to trust an arbitrary HTTP server. The host still verifies the
signed sequence, namespace, schema restrictions, payload and actor signature
before sequencing; the Chess fold still decides seat and move authority.

## Reuse the existing boundaries

Use the existing bounded games, board and legal-move reads. The native agent
may poll the board initially; it does not need to join live presence or implement
chat to play. Reads continue to exclude locally pending forge history.

Extend the existing typed game-action prepare/submit adapter for the current
game operations needed by the CLI and MCP: create, join, move, resign and draw
operations. Create is needed for a rematch while the service remains running.
Factor the existing application act builders where necessary; do not change
Chess schemas, legal moves, seat authority or the application fold. Existing
browser join/move requests keep their behavior. Unsupported server-mode tools
must say so explicitly and must not fall back to repository writes. Identity
tools gain no new signing or delegation authority under this work.

Preparation returns the action echo, canonical signing bytes, repository
binding and a bounded replay description. The client compares every chosen
field, including game, actor, move, predecessor and invitation data, before
signing. It uses the existing host signing-byte helper. No private key crosses
HTTP. A move retains the position the agent examined; a changed board requires
a new choice, not a silent substitution of the latest predecessor.

The current browser draft is an opaque random handle held in a 128-entry,
five-minute in-memory table. Reusing that handle alone cannot make an agent's
lost-response retry survive a service restart. Native submissions therefore
need a bounded, typed replay form alongside the existing browser form. Retain
the original action fields, original predecessor and explicit idempotency key.
The service reconstructs the same application act and canonical prepared value
from those fields and verifies the original signature through the existing
host path. Do not expose a generic opaque prepared-act or arbitrary-schema
append endpoint: that would bypass the application's game-action boundary.
Reject mixed native/browser forms and altered replay content.

The native replay must not depend on the old in-memory draft or on a new board
read. In particular, an accepted exact retry must reach existing idempotent
acceptance before a current-position check can reject the already accepted
move. A new, conflicting action keeps the ordinary refusal behavior. Reuse the
repository's existing append and forge-confirmation path; returning a local
record before exact forge confirmation remains forbidden.

## Failure and retry behavior

Use finite connection, response, body and retry bounds; document their concrete
values in the implementation. Retain existing HTTP body and application payload
ceilings and bound native replay admission. Do not add an unbounded action queue.

Before the first submit, atomically retain one bounded pending attempt per
configured agent key, in a private file protected by the existing custody
rules. It contains the exact action, sequence binding, idempotency key and
signature, not the private key. Invitation data and replay authority make this
file private too. MCP reconnect and CLI retry load that same attempt. While its
outcome is unknown, refuse a different mutation rather than overwrite it.
A confirmed result closes the pending attempt. This is recovery of the current
attempt, not an unbounded cache of all past commands.

A timeout, disconnect or uncertain append returns “outcome not confirmed” and
the retained retry handle. Retry exactly the same signed action, with bounded
backoff. Do not prepare another move, mint another idempotency key, or acquire a
local writer. On a service restart, verify the same genesis, resend the retained
attempt and recover its original accepted record or explicit refusal. A wrong
server, changed replay, expired or unusable protocol state must refuse safely.
Do not infer success merely because the board resembles the intended result.
The CLI and MCP must surface the same durable record, decision and confirmed
frontier as the service, including an ineffective gameplay decision.

## Small implementation and acceptance plan

1. Add the typed native replay form and service description using the existing
   owner and host APIs. Keep browser behavior, body bounds, signed authority,
   pending-forge filtering and exact confirmation intact. Prove wrong genesis,
   actor, payload, signature, mixed form and unsupported action refusals.
2. Add the common CLI/MCP service client and secure standalone key setup. Keep
   the explicit destination and one pending-attempt rule. Test refusal without
   any local writer opening, including when a local repository also exists.
3. Exercise one real browser human and one actual CLI/MCP agent against one
   uninterrupted local service: setup, agent join, alternating legal moves,
   refresh, terminal result, and a new named rematch with the same keys.
   Record steps, copied values, screenshots and public record/decision evidence.
   Two browser players are not this acceptance test.
4. Drop an actual successful submit response, then retry the retained attempt
   both before and after restarting the service and adapter. Require the same
   record, unchanged duplicate count and exact forge-confirmed acknowledgement.
   Move the board meanwhile and ensure replay is not mistaken for a new move.
   Also test a stale new move, wrong seat/key, disconnected service, missing
   forge confirmation, missing key, malformed or oversized response, and wrong
   repository. Remove the replay or no-local-writer guard as an omission control
   and demonstrate the corresponding test fails.

Implementation needs a durable request after this decision is adopted and
reviewed. Its architecture and transport reference updates, exact candidate,
independent security/simplification review, sealed delivery and cleanup belong
to that request. The original playability review stays open for actual browser
acceptance. The public-host decision and activation gates remain separate.
