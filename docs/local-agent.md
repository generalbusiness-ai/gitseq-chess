# Play through the running service with your agent

Run `chess serve` continuously. It owns the repository writer; your agent
connects over loopback HTTP and signs with its own existing private key.
It needs no checkout, sequencer key, forge credential, live-room session or
identity delegation. This mode is for an operator-selected service on the same
host. The service is trusted to encode the chosen intent truthfully; matching
an echo or genesis does not authenticate a malicious service.

## Set up one agent

Create a private directory and explicitly create the agent's key once:

```sh
mkdir -m 700 agent-custody
./chess keygen --key agent-custody/player.key
curl --fail http://127.0.0.1:8080/v1/service
```

Key creation prints the path and public actor fingerprint, never private key
material. It refuses an existing destination. Keep the key and its adjacent
`.player.key.agent` directory together. The latter holds a private public-key
pin, a persistent OS lock file and, only while needed, one signed pending
attempt. Invitation data and replay authority make that attempt private too.
The key must be a regular, singly linked file owned by this account, mode
`0400` or `0600`; its directory must not be writable by another account.
Missing, replaced, linked or unsafe keys refuse. Server mode never creates a
replacement key or obtains another key's seat.

Copy the description's `genesis`, including `git:sha1:` or `git:sha256:`, into
the configuration. The bare genesis printed by `init` and used in the Git
sequence ref is the suffix of this canonical name.

```sh
CHESS_SERVER=http://127.0.0.1:8080
CHESS_GENESIS='git:sha1:<full-genesis>'
CHESS_AGENT_KEY=agent-custody/player.key
./chess mcp --server "$CHESS_SERVER" --genesis "$CHESS_GENESIS" --key "$CHESS_AGENT_KEY"
```

Configure those arguments as the agent's stdio MCP server. This mode offers
`list_games`, `show_board`, `legal_destinations`, `create`, `join`, `move`,
`resign`, `draw_offer`, `draw_accept` and `retry`. Identity tools are explicitly
unsupported. Each connection attempt checks the expected genesis and protocol.
The URL must be HTTP with a literal loopback IP, with no credentials, path,
query or fragment. Redirects and ambient proxies are disabled. An explicit
`--repo` together with `--server` refuses. An unavailable service never causes
local repository access. As with other Chess commands, unset inherited
`GIT_*` variables before launch.

## Start a game and answer a move

For example, create the agent's game as Black using the same connection:

```sh
./chess create --server "$CHESS_SERVER" --genesis "$CHESS_GENESIS" \
  --key "$CHESS_AGENT_KEY" --name "Evening game" --color black
```

The returned `record` is the game identifier. Open the service in your browser,
choose that game, join as White and play the first move. Keep that tab open:
a reload loses an unanchored browser key. The agent reads the resulting board:

```sh
./chess board --server "$CHESS_SERVER" --genesis "$CHESS_GENESIS" \
  --key "$CHESS_AGENT_KEY" --game '<game-id>'
./chess move --server "$CHESS_SERVER" --genesis "$CHESS_GENESIS" \
  --key "$CHESS_AGENT_KEY" --game '<game-id>' --move e7e5 \
  --predecessor '<last_move from the board examined>'
```

The move, resignation and draw-offer commands require the explicit
`predecessor`. `draw_accept` requires `--offer` from the board's `draw_offer`.
MCP uses the corresponding `predecessor` and `offer` fields. A changed position
refuses preparation; choose again after reading it. A race after signing can
still record an ineffective action, which the response reports honestly.

CLI `games` accepts `--after` and `--limit` (1–100); `legal` accepts `--game`
and `--from`. CLI `join` accepts `--secret-file`, and `create` accepts
`--join-secret-file` or `--invite-key`. A secret filename of `-` reads standard
input. MCP uses `secret` for a join and `join_secret` for creation. A signed
join makes its invitation secret public in the record. A rematch is another
named `create`, with the same player keys and a new game identifier.

## Recover an unconfirmed action

Before submitting, the agent atomically saves the exact typed action,
genesis, original predecessor, retry key, public key and signature. Concurrent
commands cannot replace it. While the outcome is unknown, another mutation
refuses; board reads remain available when the key is not in use by a command.
The client makes at most two submit attempts, with 200 ms between them. It
never prepares a replacement or refreshes the predecessor during retry.

If the result says **outcome not confirmed**, restore the same service and key,
then use the MCP `retry` tool with `{}` or:

```sh
./chess retry --server "$CHESS_SERVER" --genesis "$CHESS_GENESIS" \
  --key "$CHESS_AGENT_KEY"
```

Retry survives both agent and service restarts. It recovers the original
record even after the board advances. A confirmed response includes `record`,
`effective`, optional `reason`, `head`, `depth` and canonical `genesis`.
An ineffective gameplay decision is a confirmed result and closes the attempt.
In forge mode, success requires exact remote confirmation. Malformed replies,
wrong bindings, damaged pending state and uncertain delivery retain the attempt
and refuse safely. Preserve that state for investigation; deleting it or
choosing a new retry key does not undo an append.

The signed replay has no five-minute browser-draft expiry. It remains usable
while the service supports `chess-agent@1`, the same sequence and existing
Chess vocabulary. Browser draft recovery keeps its separate existing lifetime.

## Protocol and bounds

`GET /v1/service` returns canonical genesis, `chess-agent@1` and the supported
operation list. Reads reuse `/v1/games`, `/v1/board` and `/v1/legal`, always at
the confirmed frontier. `POST /v1/actions/native/prepare` accepts the closed
native game-action form and returns its complete echo, canonical prepared value
and host signing bytes. The client compares every chosen echo field and the
encoded application payload, then uses `host.ActorSigningBytes` before signing
locally. The trusted service supplies the canonical intent; the client does
not implement another kernel encoder.

`POST /v1/actions/native/submit` accepts only that typed action and signature.
The service reconstructs the same act using application builders and verifies
it through the public host. No arbitrary schema or opaque prepared value can
be submitted. Accepted replay reaches host idempotency before any new-position
check. Reconstruction can proceed while forge delivery is pending; append
still enforces the existing confirmation boundary. Mixed browser/native forms,
unknown fields, trailing JSON, bad signatures and cross-action fields refuse.
Native POSTs use the browser's existing Host, Origin, Fetch Metadata and JSON
content-type guards. A busy action adapter refuses immediately; it has no
native draft table or queue.

| Bound | Value |
| --- | --- |
| HTTP request body / encoded application payload | 32 KiB / 8 KiB |
| Invitation secret / idempotency key | 4 KiB / 128 bytes |
| Private pending file | One per configured key path, at most 32 KiB |
| Client response body / response headers | 1 MiB / 16 KiB |
| Connection / response-header / whole HTTP request timeout | 2 s / 5 s / 10 s |
| Client connections per service / idle connection lifetime | 1 / 15 s |
| Submit attempts / backoff | 2 / 200 ms |
| Server header / body-read / idle timeout | 5 s / 10 s / 45 s |
| Forge confirmation | Existing 3 attempts, 12 s overall; see the forge recipe |

Each submit attempt also performs a bounded service-description request. A
fresh mutation uses at most six HTTP requests, so its network waits total at
most 60.2 seconds; explicit retry uses at most four (40.2 seconds). Context
cancellation can end those waits earlier. File sync errors prevent submission
or retain the confirmed attempt for retry. These process-restart tests do not
claim power-loss durability or public hosting readiness.

Automated tests run actual CLI/MCP processes against an owning service, finish
a game and rematch, drop successful responses, advance the board, restart both
sides and compare the original record and forge frontier. They also exercise
forge outages and malformed, unauthorized and unavailable paths. The separate
actual browser-human plus own-agent acceptance remains required; automated
transport tests do not complete that workroom commitment.
