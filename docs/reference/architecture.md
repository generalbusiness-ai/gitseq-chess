# Chess architecture

Chess is an application of Gitseq's public host. The layer numbers below
refer to Gitseq's architecture; this repository owns the chess interpretation
and its user interfaces, not the host or kernel.

| Layer | Owner and contract |
| --- | --- |
| 1–4: signed records, storage, order and public host | The pinned Gitseq module verifies binding, signatures and ordered records. Chess uses public `host`, `host/identity` and `host/live` APIs. These contracts are unchanged. |
| 5: application interpretation | `chess.go` encodes chess acts and folds verified records into games. Seat authority uses the shared identity projection. The fold remains pure and unchanged by browser actions. `JoinAct` and `MoveAct` expose the same pure encoders used by CLI/MCP; `MoveAct` takes an explicit predecessor. |
| 6: bounded reads | Projection methods provide games, positions and legal destinations. `GET /v1/board` returns the game, head, server-rendered square data and that game's entries from the projection's bounded refusal tail. The browser draws these values without a second rules engine. |
| 7: CLI, MCP and local browser | `cmd/chess` owns local key custody, HTTP and UI. The browser now prepares and signs joins and moves. CLI/MCP game semantics and existing identity/live surfaces remain unchanged. |

## Browser actions

`POST /v1/actions/prepare` accepts `action` (`join` or `move`), `game`,
`actor_key` (base64 public Ed25519 key), and the action's fields. A join may
carry `secret`; a move requires `move` (UCI) and `predecessor` (the last move
record shown by the board). Cross-action fields refuse. The named game must
exist, and a move prepared against another predecessor refuses with 409 so the
player must make a new choice. This precheck does not replace fold judgment:
another actor can move between preparation and submission.

The service uses the application's pure encoder, public `host.Prepare` and
`host.ActorSigningBytes`. It retains the prepared intent and returns a random
draft identifier, base64 signing bytes, expiry, verified head, and an echo of
action/game/move/secret/predecessor/actor. Before signing, the UI checks all six
echo fields against the player's choice, checks the draft and expiry, and
shows a confirmation. It signs only those host-produced bytes with its
nonexportable tab key. There is no browser kernel encoder or service-held
player key. Trust in the local service includes truthfully supplying the
prepared bytes and echo, just as it includes serving the JavaScript itself.

`POST /v1/actions/submit` accepts only the draft identifier, public actor key
and signature. The service finds its cached intent, checks the actor and
signature over the exact host bytes, and calls public `host.AppendSigned`.
It returns the recorded identifier, effective/refused fold judgment, reason,
head and depth. Invalid signatures or mismatched actors append nothing. A
well-signed but stale, unauthorized or losing join act can be recorded and
refused; the UI refreshes the accepted position and lists that refusal.

Both endpoints require a loopback Host and JSON content type, reject an
inconsistent Origin or cross-site Fetch Metadata, reject query parameters,
unknown fields and trailing JSON, and bound the body to 32 KiB. The prepared
application payload is at most 8 KiB. This is the existing local browser trust
boundary, not a public hosting or multi-user authentication service.

## Retry and key lifetime

At most 128 pending or completed game drafts live in service memory for five
minutes. Saturation refuses before repository opening; expired slots are
reclaimed on access. Submissions serialize within this draft service. The
cache never stores a private player key or persists across restart.

The browser retains the original submission after an ambiguous network or
service failure and offers an explicit retry. It never re-prepares it or
re-signs a different intent. After success the service caches the record;
retries return it without another append. If append succeeded but its response
was lost before caching, the unchanged host idempotency key recovers that act.
Unknown/expired drafts require a fresh read and an explicit new action. A
local cancel clears only the pending UI; it cannot undo a recorded act.

Live board refresh updates the existing page and retains its key. Reload or
tab closure loses the key and live credential. An unanchored seat remains tied
to its exact key; another tab cannot recover it. Shared host anchoring and
scope/expiry/revocation rules remain the source of recovery authority. The UI
refreshes identity before preparing an action and warns about unanchored key
loss. Invitation fragments are cleared from the address and held only in
memory until the join is submitted; the signed join reveals its secret.

## Delivery boundary and verification

This stage provides local browser play. Process-wide writer ownership,
forge-confirmed visibility, container deployment and the two-browser hosted
acceptance belong to the subsequent adopted browser-delivery stage. The
current draft mutex makes no claim to coordinate other CLI/MCP processes or
remote durability.

Go tests exercise join races, stale moves, exact retry depth, signature/actor
and cross-game refusal, restart/expiry/saturation, body/payload limits and
origin guards. A Go-to-WebCrypto fixture runs the actual UI echo guard and
proves identical Ed25519 signatures over host bytes, private-key export
refusal and acceptance through the real HTTP handler. Node tests mutate every
echo field. Browser acceptance additionally exercises a real tab joining,
retaining its key across an incoming CLI move, signing a move, retrying a
lost response without another record, and losing authority after key loss.
