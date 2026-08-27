const test = require("node:test");
const assert = require("node:assert/strict");

const {actorLabel, createActorLabel, promptedDisplayName} = require("./ui/app.js");

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
