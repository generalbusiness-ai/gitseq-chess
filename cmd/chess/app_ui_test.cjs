const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const {
  actorLabel,
  createActorLabel,
  promptedDisplayName,
  identityPresentation,
  acceptsOAuthPopupMessage,
  signNostrEvent,
  sessionKeyNotice,
} = require("./ui/app.js");

test("a supplied display name is the visible live label", () => {
  assert.equal(actorLabel("abcdef0123456789", "Alice", 12), "Alice");
});

test("a missing display name falls back to the truncated fingerprint", () => {
  assert.equal(actorLabel("abcdef0123456789", "", 12), "abcdef012345");
});

test("markup in a display name is assigned as text, never markup", () => {
  const writes = [];
  const ownerDocument = {
    createElement(tagName) {
      return {
        tagName,
        set textContent(value) { writes.push(["textContent", value]); },
        set innerHTML(value) { writes.push(["innerHTML", value]); },
      };
    },
  };
  const markup = "<img src=x onerror=alert(1)>";
  const label = createActorLabel(ownerDocument, "abcdef0123456789", markup, 12);
  assert.equal(label.tagName, "span");
  assert.deepEqual(writes, [["textContent", markup]]);
});

test("authority labels retain the full verifiable fingerprint", () => {
  const fingerprint = "abcdef0123456789";
  assert.equal(actorLabel(fingerprint, "Alice", 0, true), `Alice (${fingerprint})`);
  assert.equal(actorLabel(fingerprint, "", 0, true), fingerprint);
});

test("the join prompt refuses empty, control, and overlong names", () => {
  assert.equal(promptedDisplayName(null), undefined);
  assert.equal(promptedDisplayName("  Alice  "), "Alice");
  assert.throws(() => promptedDisplayName("   "), /must not be empty/);
  assert.throws(() => promptedDisplayName("Alice\nBob"), /control characters/);
  assert.throws(() => promptedDisplayName("Alice\n"), /control characters/);
  assert.throws(() => promptedDisplayName("a".repeat(65)), /at most 64/);
});

test("identity display keeps vouching and verification as separate axes", () => {
  const anchored = identityPresentation({
    actor: "session-new",
    anchored: true,
    display: "github:42 @alice",
    identity: {scheme: "github", subject: "42", handle: "alice"},
    vouching: "witnessed",
    verification: "live-lookup",
  });
  assert.equal(anchored.title, "Anchored session key");
  assert.equal(anchored.identity, "github:42 @alice");
  assert.equal(anchored.vouching, "witnessed");
  assert.equal(anchored.verification, "live-lookup");
  assert.match(anchored.recovery, /same persistent identity and chess scope/);
});

test("unanchored display does not imply that a lost seat can be recovered", () => {
  const unanchored = identityPresentation({actor: "session-new", anchored: false});
  assert.equal(unanchored.title, "Unanchored session key");
  assert.equal(unanchored.vouching, "unvouched");
  assert.equal(unanchored.verification, "unverified");
  assert.match(unanchored.recovery, /cannot be recovered/);
  assert.doesNotMatch(unanchored.recovery, /may recover/);
});

test("NIP-07 receives the exact event object returned by the server", async () => {
  const exactEvent = {kind: 20000, created_at: 1720000000, tags: [], content: "nostr:delegation:exact"};
  let received;
  const signed = {id: "event-id", sig: "root-signature"};
  const nostr = {async signEvent(event) { received = event; return signed; }};
  assert.equal(await signNostrEvent(nostr, exactEvent), signed);
  assert.equal(received, exactEvent);
});

test("NIP-07 absence is reported without fabricating a proof", async () => {
  await assert.rejects(() => signNostrEvent(undefined, {}), /NIP-07 signer is not available/);
});

test("OAuth completion accepts only the exact popup and same origin", () => {
  const popup = {};
  const message = {type: "gitseq-chess:github-oauth", status: "complete"};
  assert.equal(acceptsOAuthPopupMessage({origin: "https://chess.test", source: popup, data: message}, popup, "https://chess.test"), true);
  assert.equal(acceptsOAuthPopupMessage({origin: "https://evil.test", source: popup, data: message}, popup, "https://chess.test"), false);
  assert.equal(acceptsOAuthPopupMessage({origin: "https://chess.test", source: {}, data: message}, popup, "https://chess.test"), false);
  assert.equal(acceptsOAuthPopupMessage({origin: "https://chess.test", source: popup, data: {...message, type: "identity"}}, popup, "https://chess.test"), false);
  assert.equal(acceptsOAuthPopupMessage({origin: "https://chess.test", source: popup, data: {...message, status: "token"}}, popup, "https://chess.test"), false);
});

test("new fingerprints are distinguished from lost unanchored keys", () => {
  const lost = "old-session-fingerprint";
  const fresh = "new-session-fingerprint";
  const refusal = sessionKeyNotice(lost, fresh, false);
  assert.match(refusal, new RegExp(fresh));
  assert.match(refusal, new RegExp(lost));
  assert.match(refusal, /not recoverable/);
  assert.doesNotMatch(refusal, /restored|same key/);
  assert.equal(sessionKeyNotice(fresh, fresh, false), "");
});

test("a replacement anchored key is described as requiring re-anchoring, not as automatic recovery", () => {
  const notice = sessionKeyNotice("old-session", "new-session", true);
  assert.match(notice, /Re-anchor/);
  assert.doesNotMatch(notice, /recovered|restored/);
});

test("both browser views state tab-key loss and expose both identity axes", () => {
  for (const name of ["lobby.html", "game.html"]) {
    const html = fs.readFileSync(path.join(__dirname, "ui", name), "utf8");
    assert.match(html, /id="identity-fingerprint"/);
    assert.match(html, /id="identity-vouching"/);
    assert.match(html, /id="identity-verification"/);
    assert.match(html, /unanchored seat cannot be recovered/i);
  }
});
