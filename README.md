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
key. Board, HTTP, and MCP responses never include key material.

Create a game with an open seat:

```sh
./chess create --repo game-data --color white
```

Or create a secret invitation. The log contains only the SHA-256 hash until the
join is submitted; the returned link keeps the secret in its URL fragment.

The secret is read from a file, or from standard input when the file is named
`-`. There is no flag that carries the secret itself, because a command
argument is visible in the process table to every account on the machine for as
long as the command runs.

```sh
printf 'shared-once' > invitation.secret
chmod 600 invitation.secret
./chess create --repo game-data --color white --join-secret-file invitation.secret
```

Naming a source that turns out to be empty is an error rather than an absent
secret, so a mistyped path cannot quietly create an invitation anyone can
accept. Omit the flag when an open seat is what you want.

Use another key file for the opponent, then play in UCI notation:

```sh
./chess join --repo game-data --key bob.key --game '<create-record>' --secret-file invitation.secret
./chess move --repo game-data --game '<create-record>' --move e2e4
./chess move --repo game-data --key bob.key --game '<create-record>' --move e7e5
./chess board --repo game-data --game '<create-record>'
```

Give each player or agent a different key file. A command reports both the
accepted record identifier and the fold's `effective` decision; illegal moves
and lost join races remain in the history with a short refusal reason.

### Where the player key lives

By default the key is `chess/player.key` beneath the repository's Git common
directory, and it never leaves it. The directory is opened once and every name
is resolved against that open directory, so a path component replaced while the
command runs cannot redirect where the key is read or written. A symbolic link
standing in for the key file or for the `chess` directory is refused outright,
even one pointing back inside the same directory.

A new key is published by writing it beside its destination and linking it into
place, so the real name never holds a half-written key, and a second process
racing to create the first key cannot overwrite the winner. The file is set to
mode `0600` explicitly rather than left to the process umask.

`--key` names a different file, and that is an escape hatch with a boundary
worth stating plainly. Choosing the location, and everything above the file's
own parent directory, is yours: the program pins that parent and does not
inspect the path that led to it, so ancestor symbolic links are your
responsibility. The parent must already exist. What the program still refuses,
wherever you put the file: a final component that is a symbolic link or is not
a regular file, a file readable by anyone but its owner, a file that changes
identity between being named and being opened, and any publication that is not
exclusive.

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
