function actorLabel(actor, displayName, fallbackLength, authority = false) {
  if (authority) return displayName ? `${displayName} (${actor})` : actor;
  return displayName || actor.slice(0, fallbackLength);
}

function createActorLabel(ownerDocument, actor, displayName, fallbackLength) {
  const label = ownerDocument.createElement(displayName ? "span" : "code");
  label.className = "actor-label";
  label.textContent = actorLabel(actor, displayName, fallbackLength);
  label.title = actor;
  return label;
}

function promptedDisplayName(value) {
  if (value === null) return undefined;
  if (/[\u0000-\u001f\u007f-\u009f]/u.test(value)) throw new Error("Display name must not contain control characters.");
  const name = value.trim();
  if (!name) throw new Error("Display name must not be empty.");
  if ([...name].length > 64) throw new Error("Display name must be at most 64 characters.");
  return name;
}

function identityPresentation(status) {
  const actor = status && typeof status.actor === "string" ? status.actor : "";
  if (!status || status.anchored !== true || !status.identity) {
    return {
      title: "Unanchored session key",
      identity: "No persistent identity is linked to this key.",
      vouching: "unvouched",
      verification: "unverified",
      recovery: "If this tab key is lost before it is anchored and used for an effective chess act, its seat cannot be recovered.",
      actor,
    };
  }
  const scheme = typeof status.identity.scheme === "string" ? status.identity.scheme : "identity";
  const subject = typeof status.identity.subject === "string" ? status.identity.subject : "unknown";
  const handle = typeof status.identity.handle === "string" && status.identity.handle ? `@${status.identity.handle} · ` : "";
  return {
    title: "Anchored session key",
    identity: status.display || `${handle}${scheme}:${subject}`,
    vouching: status.vouching || "unvouched",
    verification: status.verification || "unverified",
    recovery: "A fresh key anchored to this same persistent identity and chess scope may recover a seat that was identity-bound.",
    actor,
  };
}

function acceptsOAuthPopupMessage(event, popup, origin) {
  if (!event || event.origin !== origin || event.source !== popup) return false;
  const message = event.data;
  return Boolean(message && typeof message === "object"
    && message.type === "gitseq-chess:github-oauth"
    && (message.status === "complete" || message.status === "error"));
}

async function signNostrEvent(nostr, event) {
  if (!nostr || typeof nostr.signEvent !== "function") throw new Error("A NIP-07 signer is not available.");
  return nostr.signEvent(event);
}

async function startGitHubOAuth(postJSON, subtle, privateKey, actorKey, scope, notAfter, decodeBase64, encodeBase64) {
  const prepared = await postJSON("/v1/identity/github/challenge", {
    actor_key: actorKey,
    scope,
    not_after: notAfter,
  });
  const signature = await subtle.sign("Ed25519", privateKey, decodeBase64(prepared.signing_bytes));
  return postJSON("/v1/identity/github/start", {
    actor_key: actorKey,
    scope,
    not_after: notAfter,
    challenge: prepared.challenge,
    actor_signature: encodeBase64(signature),
  });
}

function sessionKeyNotice(previousActor, currentActor, anchored) {
  if (!previousActor || previousActor === currentActor) return "";
  return anchored
    ? `This tab has a new key (${currentActor}). Re-anchor it to the same persistent identity before relying on seat recovery.`
    : `This tab has a new key (${currentActor}). It is not the lost key (${previousActor}); an unanchored seat held by the lost key is not recoverable.`;
}

// Check the host's action echo against the user's choice before asking WebCrypto
// to sign. The host remains the sole intent encoder.
function checkedGameDraft(prepared, expected, nowSeconds) {
  const fields = ["action", "game", "move", "secret", "predecessor", "actor"];
  if (!prepared || !prepared.echo || Object.keys(prepared.echo).length !== fields.length
      || fields.some((key) => prepared.echo[key] !== expected[key])
      || !/^[a-f0-9]{48}$/.test(prepared.draft || "")
      || typeof prepared.signing_bytes !== "string" || !prepared.signing_bytes.length
      || prepared.signing_bytes.length > 65536
      || !Number.isInteger(prepared.expires) || prepared.expires <= nowSeconds) {
    throw new Error("The prepared action differs from your choice or has expired. Choose the action again.");
  }
  return prepared.signing_bytes;
}

if (typeof module !== "undefined") module.exports = {
  checkedGameDraft,
  actorLabel,
  createActorLabel,
  promptedDisplayName,
  identityPresentation,
  acceptsOAuthPopupMessage,
  signNostrEvent,
  startGitHubOAuth,
  sessionKeyNotice,
};

(() => {
  "use strict";

  if (typeof document === "undefined") return;

  const body = document.body;
  if (body.dataset.view === "oauth-callback") {
    if (body.dataset.status === "complete" && window.opener) {
      window.opener.postMessage({type: "gitseq-chess:github-oauth", status: "complete"}, window.location.origin);
      window.close();
    }
    return;
  }

  let durableHead = body.dataset.head || "";
  let predecessor = body.dataset.predecessor || "";
  let pendingGameAction = null;
  let identityAnchored = false;
  let actionBusy = false;
  const game = body.dataset.game || "";
  let keyPair = null;
  let keyCreation = null;
  let actorKey = "";
  let actorFingerprint = "";
  let credential = "";
  let displayName;
  let role = "watcher";
  let selectedFrom = "";
  let destinations = [];
  let cursor = {durable: {genesis: "", head: "", depth: 0}, live: {generation: "", position: 0}};
  const seenMotion = new Set();

  const helpDialog = document.querySelector("[data-help-dialog]");
  document.querySelectorAll("[data-help-open]").forEach((button) => {
    button.addEventListener("click", () => {
      if (helpDialog && typeof helpDialog.showModal === "function") helpDialog.showModal();
    });
  });

  function bytesToBase64(value) {
    const bytes = new Uint8Array(value);
    let binary = "";
    bytes.forEach((byte) => { binary += String.fromCharCode(byte); });
    return window.btoa(binary);
  }

  function base64ToBytes(value) {
    const binary = window.atob(value);
    return Uint8Array.from(binary, (character) => character.charCodeAt(0));
  }

  async function post(path, value) {
    const response = await fetch(path, {
      method: "POST",
      headers: {Accept: "application/json", "Content-Type": "application/json"},
      credentials: "omit",
      body: JSON.stringify(value),
    });
    if (!response.ok) {
      const error = new Error((await response.text()).trim() || "request failed");
      error.status = response.status;
      throw error;
    }
    return response.json();
  }

  function scopeFromControls() {
    const scope = document.getElementById("identity-scope");
    return !game || (scope && scope.value === "chess") ? "chess" : `chess:${game}`;
  }

  function expiryFromControls() {
    const input = document.getElementById("identity-expiry");
    const hours = Number(input ? input.value : 168);
    if (!Number.isInteger(hours) || hours < 1 || hours > 720) {
      throw new Error("Identity expiry must be between 1 and 720 hours.");
    }
    return Math.floor(Date.now() / 1000) + hours * 60 * 60;
  }

  function setIdentityMessage(message, failure = false) {
    const element = document.getElementById("identity-message");
    if (!element) return;
    element.textContent = message;
    element.classList.toggle("refused", failure);
  }

  function renderIdentity(statusResult) {
    const view = identityPresentation(statusResult);
    identityAnchored = Boolean(statusResult && statusResult.anchored === true);
    const title = document.getElementById("identity-title");
    const persistent = document.getElementById("identity-persistent");
    const vouching = document.getElementById("identity-vouching");
    const verification = document.getElementById("identity-verification");
    const recovery = document.getElementById("identity-recovery");
    const fingerprint = document.getElementById("identity-fingerprint");
    if (title) title.textContent = view.title;
    if (persistent) persistent.textContent = view.identity;
    if (vouching) vouching.textContent = view.vouching;
    if (verification) verification.textContent = view.verification;
    if (recovery) recovery.textContent = view.recovery;
    if (fingerprint) fingerprint.textContent = view.actor || actorFingerprint || "No tab key yet";
    const endorse = document.querySelector("#agent-endorsement-form button");
    if (endorse) endorse.disabled = !(statusResult && statusResult.anchored === true);
  }

  async function refreshIdentity() {
    if (!actorKey) {
      renderIdentity(null);
      return null;
    }
    const result = await post("/v1/identity/status", {actor_key: actorKey});
    actorFingerprint = result.actor || actorFingerprint;
    renderIdentity(result);
    return result;
  }

  async function ensureSessionKey() {
    if (keyCreation) return keyCreation;
    if (keyPair) { if (!actorFingerprint) await refreshIdentity(); return; }
    keyCreation = createSessionKey();
    try { await keyCreation; } finally { keyCreation = null; }
  }

  async function createSessionKey() {
    setIdentityMessage("Creating a temporary, non-exportable signing key in this tab…");
    keyPair = await window.crypto.subtle.generateKey({name: "Ed25519"}, false, ["sign", "verify"]);
    const raw = new Uint8Array(await window.crypto.subtle.exportKey("raw", keyPair.publicKey));
    actorKey = bytesToBase64(raw);
    const create = document.getElementById("create-tab-key");
    const github = document.getElementById("anchor-github");
    const nostr = document.getElementById("anchor-nostr");
    if (create) { create.disabled = true; create.textContent = "Tab key ready"; }
    if (github) github.disabled = false;
    if (nostr) nostr.disabled = false;
    await refreshIdentity();
    setIdentityMessage("This public fingerprint identifies the key held only by this tab. A reload will create a different one.");
  }

  async function signIdentityDraft(prepared) {
    const signature = await window.crypto.subtle.sign("Ed25519", keyPair.privateKey, base64ToBytes(prepared.signing_bytes));
    return {draft: prepared.draft, actor_key: actorKey, actor_signature: bytesToBase64(signature)};
  }

  function clearDestinations() {
    document.querySelectorAll(".square").forEach((square) => square.classList.remove("selected", "legal"));
    selectedFrom = "";
    destinations = [];
  }

  async function signDraft(draft, signingBytes) {
    const signature = await window.crypto.subtle.sign("Ed25519", keyPair.privateKey, base64ToBytes(signingBytes));
    return {draft, actor_key: actorKey, actor_signature: bytesToBase64(signature)};
  }

  async function publishMotion(phase, from, to = "") {
    if (!credential || !keyPair) return;
    try {
      await post("/v1/live/motion", {credential, game, actor_key: actorKey, display_name: displayName, phase, from, to});
    } catch (error) {
      const status = document.getElementById("live-status");
      if (status) status.textContent = `Motion preview refused: ${error.message}`;
    }
  }

  const invitationSecret = game ? (new URLSearchParams(window.location.hash.slice(1)).get("secret") || "") : "";
  if (game && window.location.hash) window.history.replaceState(null, "", window.location.pathname + window.location.search);

  async function refreshBoard() {
    if (!game) return;
    const endpoint = new URL("/v1/board", window.location.origin);
    endpoint.searchParams.set("game", game);
    const response = await fetch(endpoint, {headers: {Accept: "application/json"}, credentials: "omit"});
    if (!response.ok) throw new Error("The durable board could not be refreshed.");
    const result = await response.json();
    if (!result.game || result.game.id !== game || !Array.isArray(result.squares)) throw new Error("Unexpected game response.");
    const changed = durableHead !== result.head;
    durableHead = result.head;
    predecessor = result.game.last_move || "";
    body.dataset.head = durableHead;
    body.dataset.predecessor = predecessor;
    if (changed) clearDestinations();
    for (const square of result.squares) {
      const button = document.querySelector(`[data-square="${square.Name}"]`);
      if (!button) continue;
      button.setAttribute("aria-label", square.Label);
      button.querySelector("span").textContent = square.Piece;
    }
    const position = result.game;
    document.querySelector(".board").dataset.fen = position.fen;
    document.querySelector(".fen").textContent = position.fen;
    document.querySelector(".game-status").textContent = position.status + (position.turn ? ` · ${position.turn === "w" ? "white" : "black"} to move` : "");
    const outcome = document.querySelector(".outcome");
    outcome.hidden = !position.outcome;
    outcome.textContent = position.outcome ? `${position.outcome} by ${position.method}` : "";
    const seats = document.querySelectorAll(".seats dd");
    [position.white, position.black].forEach((actor, index) => { seats[index].textContent = actor || (position.admission_open ? "Open" : "Restricted"); });
    document.getElementById("move-count").textContent = `Moves ${position.moves}`;
    document.getElementById("last-move").textContent = position.last_move_uci ? `Last move ${position.last_move_uci}` : "";
    document.getElementById("draw-offer").textContent = position.draw_offer ? `Draw offered by ${position.offered_by}` : "";
    document.getElementById("join-game").hidden = Boolean(position.white && position.black);
    const refusals = document.querySelector(".game-refusals");
    const list = refusals.querySelector("ol");
    list.replaceChildren();
    for (const refusal of result.refusals || []) {
      const item = document.createElement("li");
      const reason = document.createElement("strong"); reason.textContent = refusal.reason;
      const record = document.createElement("code"); record.textContent = refusal.record;
      item.append(reason, " ", record); list.append(item);
    }
    refusals.hidden = !list.children.length;
    if (changed && pendingGameAction && !pendingGameAction.submission && !actionBusy) {
      pendingGameAction = null;
      document.getElementById("action-confirmation").hidden = true;
      help.textContent = "The position changed before signing. Choose your action again.";
    }
  }

  async function prepareGameAction(action, move = "") {
    if (actionBusy || (pendingGameAction && pendingGameAction.submission)) return;
    actionBusy = true;
    pendingGameAction = null;
    document.getElementById("action-confirmation").hidden = true;
    try {
      await ensureSessionKey();
      await refreshIdentity();
      const expected = {action, game, move, secret: action === "join" ? invitationSecret : "", predecessor: action === "move" ? predecessor : "", actor: actorFingerprint};
      const {actor, ...input} = expected;
      const prepared = await post("/v1/actions/prepare", {...input, actor_key: actorKey});
      checkedGameDraft(prepared, expected, Math.floor(Date.now() / 1000));
      pendingGameAction = {prepared, expected, submission: null};
      const gameLabel = document.querySelector("h1").textContent;
      document.getElementById("action-description").textContent = action === "join" ? `Join ${gameLabel}` : `Move ${move} in ${gameLabel}`;
      document.getElementById("action-key-warning").textContent = identityAnchored
        ? "Sign with this tab's anchored key. The game checks whether this key may play."
        : "This key is unanchored. Reloading or closing this tab can permanently lose your seat. Anchor the key before joining if you need recovery.";
      document.getElementById("sign-game-action").textContent = "Sign and submit";
      document.getElementById("action-confirmation").hidden = false;
      help.textContent = "Check the action below, then sign to submit it.";
    } catch (error) { help.textContent = error.message; }
    finally { actionBusy = false; }
  }

  async function submitGameAction() {
    if (actionBusy || !pendingGameAction) return;
    const pending = pendingGameAction;
    actionBusy = true;
    const button = document.getElementById("sign-game-action");
    button.disabled = true;
    let recorded = null;
    try {
      const bytes = checkedGameDraft(pending.prepared, pending.expected, Math.floor(Date.now() / 1000));
      if (!pending.submission) pending.submission = await signDraft(pending.prepared.draft, bytes);
      const result = await post("/v1/actions/submit", pending.submission);
      recorded = result;
      pendingGameAction = null;
      document.getElementById("action-confirmation").hidden = true;
      await refreshBoard();
      help.textContent = result.effective === true ? `Accepted: ${result.record}` : `Recorded without changing the board: ${result.reason || "decision unavailable"} (${result.record})`;
    } catch (error) {
      if (recorded) {
        help.textContent = `Recorded: ${recorded.record}. The board could not be refreshed yet.`;
      } else if (error.status === 400 || pending.prepared.expires <= Math.floor(Date.now() / 1000)) {
        pendingGameAction = null;
        document.getElementById("action-confirmation").hidden = true;
        try { await refreshBoard(); } catch (_) { /* retain the last verified view */ }
        help.textContent = "The draft is unavailable or expired. Check the refreshed position and choose a new action only if still needed.";
      } else {
        button.textContent = "Retry the same signed action";
        help.textContent = `${error.message} Retrying keeps the original intent and signature.`;
      }
    } finally { actionBusy = false; button.disabled = false; }
  }

  const board = document.querySelector(".board");
  const help = document.getElementById("move-help");
  if (board && game && help) {
    const squares = new Map();
    board.querySelectorAll(".square").forEach((square) => {
      squares.set(square.dataset.square, square);
      square.addEventListener("click", async () => {
        const promotion = document.getElementById("promotion").value;
        const selectedDestination = destinations.find((destination) => destination.slice(0, 2) === square.dataset.square && (destination.length === 2 || destination.slice(2) === promotion));
        if (selectedFrom && selectedDestination) {
          const from = selectedFrom;
          clearDestinations();
          help.textContent = "Preparing your move for confirmation…";
          await publishMotion("submitting", from, selectedDestination);
          await prepareGameAction("move", from + selectedDestination);
          return;
        }
        clearDestinations();
        square.classList.add("selected");
        selectedFrom = square.dataset.square;
        const source = selectedFrom;
        help.textContent = "Checking the durable position…";
        const endpoint = new URL("/v1/legal", window.location.origin);
        endpoint.searchParams.set("game", game);
        endpoint.searchParams.set("from", source);
        try {
          const response = await fetch(endpoint, {headers: {Accept: "application/json"}, credentials: "omit"});
          if (!response.ok) throw new Error("legal-destination query failed");
          const result = await response.json();
          if (result.head !== durableHead) {
            await refreshBoard();
            return;
          }
          if (selectedFrom !== source) return;
          destinations = Array.isArray(result.destinations) ? result.destinations : [];
          destinations.forEach((destination) => {
            const target = squares.get(String(destination).slice(0, 2));
            if (target) target.classList.add("legal");
          });
          if (destinations.length) await publishMotion("dragged", source);
          help.textContent = destinations.length
            ? `Valid moves from ${source}: ${destinations.join(", ")}. Select a destination to prepare a signed move.`
            : `No valid move is available from ${source}${result.reason ? `: ${result.reason}.` : "."}`;
        } catch (_error) {
          help.textContent = "The durable position could not be checked.";
        }
      });
    });
  }

  document.getElementById("join-game")?.addEventListener("click", () => prepareGameAction("join"));
  document.getElementById("sign-game-action")?.addEventListener("click", submitGameAction);
  document.getElementById("cancel-game-action")?.addEventListener("click", () => {
    if (actionBusy) return;
    const wasSigned = Boolean(pendingGameAction?.submission);
    pendingGameAction = null;
    document.getElementById("action-confirmation").hidden = true;
    help.textContent = wasSigned ? "Local retry cleared; this does not undo a recorded act. Check the board before another action." : "Action cancelled before signing.";
  });

  function renderParticipants(participants) {
    const list = document.getElementById("participants");
    if (!list) return;
    list.replaceChildren();
    if (!participants.length) {
      const empty = document.createElement("li");
      empty.textContent = "No live participants.";
      list.append(empty);
      return;
    }
    participants.forEach((participant) => {
      const item = document.createElement("li");
      const badge = document.createElement("span");
      badge.className = "participant-role";
      badge.textContent = participant.role;
      const actor = createActorLabel(document, participant.actor, participant.display_name, 12);
      item.append(badge, actor);
      list.append(item);
    });
  }

  function renderChat(messages) {
    const list = document.getElementById("chat-log");
    if (!list) return;
    list.replaceChildren();
    if (!messages.length) {
      const empty = document.createElement("li");
      empty.textContent = "No chat messages yet.";
      list.append(empty);
      return;
    }
    messages.forEach((message) => {
      const item = document.createElement("li");
      const actor = createActorLabel(document, message.actor, message.display_name, 10);
      const text = document.createElement("span");
      text.textContent = message.text;
      item.append(actor, text);
      list.append(item);
    });
    list.scrollTop = list.scrollHeight;
  }

  function renderMotion(motions, initial, reset) {
    if (!board) return;
    if (reset) seenMotion.clear();
    motions.forEach((motion) => {
      if (motion.head !== durableHead) return;
      if (seenMotion.has(motion.id)) return;
      seenMotion.add(motion.id);
      if (initial || reset) return;
      const from = board.querySelector(`[data-square="${motion.from}"]`);
      const to = motion.to ? board.querySelector(`[data-square="${motion.to.slice(0, 2)}"]`) : null;
      if (!from || (motion.phase === "submitting" && !to)) return;
      window.requestAnimationFrame(() => {
        from.classList.add("motion-from");
        if (to) to.classList.add("motion-to");
        window.setTimeout(() => {
          from.classList.remove("motion-from");
          if (to) to.classList.remove("motion-to");
        }, 650);
      });
      const attributedActor = actorLabel(motion.actor, motion.display_name, 0, true);
      if (help) help.textContent = motion.phase === "dragged"
        ? `${attributedActor} (${motion.role}) is dragging from ${motion.from}; the durable board is unchanged.`
        : `${attributedActor} (${motion.role}) is submitting ${motion.from} to ${motion.to}; the durable board is unchanged.`;
    });
  }

  async function observeLive() {
    if (!game) return;
    for (;;) {
      const initial = !cursor.live.generation;
      try {
        const result = await post("/v1/live/observe", {credential, game, cursor, wait_ms: initial ? 1 : 20000});
        if (result.cursor.durable.head && result.cursor.durable.head !== durableHead) {
          await refreshBoard();
        }
        cursor = result.cursor;
        renderParticipants(result.participants || []);
        renderChat(result.chat || []);
        renderMotion(result.motions || [], initial, result.reset);
      } catch (_error) {
        await new Promise((resolve) => window.setTimeout(resolve, 1500));
      }
    }
  }

  const createTabKey = document.getElementById("create-tab-key");
  const githubAnchor = document.getElementById("anchor-github");
  const nostrAnchor = document.getElementById("anchor-nostr");
  const agentForm = document.getElementById("agent-endorsement-form");
  const agentSubject = document.getElementById("agent-subject");
  let githubPopup = null;

  if (createTabKey) {
    createTabKey.addEventListener("click", async () => {
      createTabKey.disabled = true;
      try {
        await ensureSessionKey();
      } catch (error) {
        keyPair = null;
        actorKey = "";
        createTabKey.disabled = false;
        setIdentityMessage(`Could not create the tab key: ${error.message}`, true);
      }
    });
  }

  window.addEventListener("message", async (event) => {
    if (!acceptsOAuthPopupMessage(event, githubPopup, window.location.origin)) return;
    githubPopup = null;
    if (event.data.status === "error") {
      setIdentityMessage("GitHub did not anchor this key. No account token was retained.", true);
      return;
    }
    try {
      await refreshIdentity();
      setIdentityMessage("GitHub anchored this tab key. The two verification axes are shown above.");
    } catch (error) {
      setIdentityMessage(`The anchor landed, but its current status could not be read: ${error.message}`, true);
    }
  });

  if (githubAnchor) {
    githubAnchor.addEventListener("click", async () => {
      githubAnchor.disabled = true;
      try {
        await ensureSessionKey();
        const popup = window.open("", "gitseq-chess-github", "popup,width=720,height=760");
        if (!popup) throw new Error("Allow the GitHub sign-in popup and try again.");
        githubPopup = popup;
        const started = await startGitHubOAuth(
          post, window.crypto.subtle, keyPair.privateKey, actorKey,
          scopeFromControls(), expiryFromControls(), base64ToBytes, bytesToBase64,
        );
        popup.location.replace(started.authorize_url);
        setIdentityMessage("Complete GitHub sign-in in the popup. The provider token is never returned to this page.");
      } catch (error) {
        if (githubPopup && !githubPopup.closed) githubPopup.close();
        githubPopup = null;
        setIdentityMessage(`GitHub anchoring could not start: ${error.message}`, true);
      } finally {
        githubAnchor.disabled = false;
      }
    });
  }

  if (nostrAnchor) {
    nostrAnchor.addEventListener("click", async () => {
      nostrAnchor.disabled = true;
      try {
        await ensureSessionKey();
        const scope = scopeFromControls();
        const notAfter = expiryFromControls();
        const template = await post("/v1/identity/nostr/template", {
          actor_key: actorKey, scope, not_after: notAfter,
        });
        const signedEvent = await signNostrEvent(window.nostr, template.event);
        const prepared = await post("/v1/identity/endorsement/prepare", {
          actor_key: actorKey,
          subject: template.actor,
          scope,
          not_after: notAfter,
          nostr: signedEvent,
        });
        const result = await post("/v1/identity/endorsement/submit", await signIdentityDraft(prepared));
        await refreshIdentity();
        setIdentityMessage(result.anchored
          ? "Nostr anchored this tab key with an in-log root signature."
          : "The signed Nostr endorsement was recorded but did not anchor this key.", !result.anchored);
      } catch (error) {
        setIdentityMessage(`Nostr anchoring was refused: ${error.message}`, true);
      } finally {
        nostrAnchor.disabled = false;
      }
    });
  }

  if (agentForm && agentSubject) {
    agentForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const submit = agentForm.querySelector("button");
      submit.disabled = true;
      try {
        await ensureSessionKey();
        const subject = agentSubject.value.trim();
        if (!subject) throw new Error("Enter the agent key fingerprint to endorse.");
        const prepared = await post("/v1/identity/endorsement/prepare", {
          actor_key: actorKey,
          subject,
          scope: scopeFromControls(),
          not_after: expiryFromControls(),
        });
        const result = await post("/v1/identity/endorsement/submit", await signIdentityDraft(prepared));
        setIdentityMessage(result.anchored
          ? `The agent key ${subject} is endorsed within the selected scope and expiry.`
          : "The endorsement was recorded but conferred no identity authority.", !result.anchored);
      } catch (error) {
        setIdentityMessage(`Agent endorsement was refused: ${error.message}`, true);
      } finally {
        submit.disabled = false;
      }
    });
  }

  const join = document.getElementById("join-live");
  const status = document.getElementById("live-status");
  const chatForm = document.getElementById("chat-form");
  const chatInput = document.getElementById("chat-text");
  if (join && game) {
    join.addEventListener("click", async () => {
      join.disabled = true;
      try {
        displayName = promptedDisplayName(window.prompt("Display name (optional; cancel to use your fingerprint):"));
        if (status) status.textContent = "Creating a temporary signing key in this tab…";
        await ensureSessionKey();
        const prepared = await post("/v1/live/session/prepare", {game, actor_key: actorKey, display_name: displayName});
        const signature = await window.crypto.subtle.sign("Ed25519", keyPair.privateKey, base64ToBytes(prepared.signing_bytes));
        const opened = await post("/v1/live/session/open", {challenge: prepared.challenge, signature: bytesToBase64(signature)});
        credential = opened.credential;
        role = opened.role;
        join.textContent = `Joined as ${role}`;
        if (status) status.textContent = `Live as ${role}. The credential and non-exportable private key exist only in this tab.`;
        if (chatInput && chatForm) {
          chatInput.disabled = false;
          chatForm.querySelector("button").disabled = false;
        }
      } catch (error) {
        displayName = undefined;
        join.disabled = false;
        if (status) status.textContent = `Could not join the live room: ${error.message}`;
      }
    });
  }

  if (chatForm && chatInput) {
    chatForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const text = chatInput.value.trim();
      if (!credential || !keyPair || !text) return;
      chatInput.disabled = true;
      try {
        const prepared = await post("/v1/live/chat/prepare", {credential, game, actor_key: actorKey, text});
        const submission = await signDraft(prepared.draft, prepared.signing_bytes);
        await post("/v1/live/chat/submit", {credential, game, submission});
        chatInput.value = "";
      } catch (error) {
        if (status) status.textContent = `Chat refused: ${error.message}`;
      } finally {
        chatInput.disabled = false;
        chatInput.focus();
      }
    });
  }

  window.setInterval(async () => {
    if (!credential || !keyPair) return;
    try {
      const renewed = await post("/v1/live/session/renew", {credential, game, actor_key: actorKey, display_name: displayName});
      role = renewed.role;
    } catch (_error) {
      credential = "";
      if (status) status.textContent = "The live lease expired. Join again to restore presence and chat.";
      if (join) { join.disabled = false; join.textContent = "Join presence and chat"; }
      if (chatInput) chatInput.disabled = true;
    }
  }, 20000);

  window.addEventListener("pagehide", () => {
    if (!credential) return;
    fetch("/v1/live/session/revoke", {
      method: "POST", headers: {"Content-Type": "application/json"},
      credentials: "omit", keepalive: true, body: JSON.stringify({credential}),
    });
  });

  if (game) observeLive();
  if (!game) {
    window.setInterval(async () => {
      const endpoint = new URL("/v1/games?limit=1", window.location.origin);
      try {
        const response = await fetch(endpoint, {headers: {Accept: "application/json"}, credentials: "omit"});
        if (!response.ok) return;
        const result = await response.json();
        if (result.head && result.head !== durableHead) window.location.reload();
      } catch (_error) {
        // The current durable view stays visible while the server is unavailable.
      }
    }, 2000);
  }
})();
