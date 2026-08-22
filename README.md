# gitseq chess

This repository is a complete chess application profile for the gitseq host.
Gitseq verifies each actor's signature and the accepted log order. This module
defines the chess vocabulary, folds that verified log into games, and decides
which recorded acts are effective.

The application is deliberately separate from gitseq. It imports only the
public `host` and `host/identity` packages, and pins the exact gitseq commit it
was built against in `go.mod`. The fold is pure: it uses record order and signed
sequencer timestamps, with no network, local clock, or mutable cache.

## Run locally

Build the one binary, then initialize a data repository:

```sh
go build -o chess ./cmd/chess
mkdir game-data
./chess init --repo game-data
./chess serve --repo game-data
```

`init` creates the repository's binding, sequencer key, and first local player
key. The default player key is created under the Git common directory with mode
`0600`; board, HTTP, and MCP responses never include key material. Hardening
custody and validation for caller-selected key paths is tracked separately from
the chess application semantics in this head.

Create a game with an open seat:

```sh
./chess create --repo game-data --color white
```

Or create a secret invitation. The log contains only the SHA-256 hash until the
join is submitted; the returned link keeps the secret in its URL fragment.

```sh
./chess create --repo game-data --color white --join-secret 'shared-once'
```

Use another key file for the opponent, then play in UCI notation:

```sh
./chess join --repo game-data --key bob.key --game '<create-record>' --secret 'shared-once'
./chess move --repo game-data --game '<create-record>' --move e2e4
./chess move --repo game-data --key bob.key --game '<create-record>' --move e7e5
./chess board --repo game-data --game '<create-record>'
```

A missing key file is created exclusively. Give each player or agent a
different file. A command reports both the accepted record identifier and the
fold's `effective` decision; illegal moves and lost join races remain in the
history with a short refusal reason.

## Read service and MCP

`chess serve` exposes bounded, read-only JSON endpoints:

- `GET /v1/games?limit=100&after=<game>`
- `GET /v1/board?game=<game>`
- `GET /v1/legal?game=<game>&from=e2`

The browser write path and UI belong to later work. This service therefore does
not accept private keys or unsigned write requests over HTTP.

`chess mcp --repo game-data --key agent.key` runs a newline-delimited JSON-RPC
MCP adapter on standard input and output. It offers bounded game listing, board
and legal-destination queries, plus create, join, move, resign, draw-offer,
draw-accept, and shared host identity anchor acts. The adapter's process owns
the chosen key; tool results contain only public record data and fold decisions.

## Durable rules

- A create chooses the creator's color and an optional opponent key or hashed
  join secret. No invitation means an intentionally open, first-valid-join seat.
- A join rests on the create. The first qualified join in log order seats.
- The first move rests on the accepted join; every later move rests on the
  preceding accepted move. Wrong-turn, stale-chain, and illegal moves are
  recorded but ineffective.
- Checkmate, stalemate, repetition, the fifty-move rule, and insufficient
  material are computed by the fold's single rules engine. There is no result
  act that a player can use to claim a win.
- Resignations and draw offers name the current move chain. A draw acceptance
  rests on the exact pending offer and only the other seated player may accept.
- `gitseq/identity-anchor@0` is shared host vocabulary; chess does not create
  its own identity system. A seat belongs to a chess-scoped anchored identity
  when one is in force at the exact create, join, or later act position;
  otherwise it belongs to the exact session key. A seated key may therefore
  play first and upgrade later, and another currently anchored key for that
  identity can recover the seat. Anchor, delegation, revocation, and move
  records that share one signed second still take force only in verified log
  order. Expiry continues to use the record's signed timestamp.

Run `go test -count=1 ./...` for fold, mutation-witness, invitation, pagination,
and real external-host integration coverage.
