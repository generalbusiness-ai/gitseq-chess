(() => {
  "use strict";

  const body = document.body;
  const durableHead = body.dataset.head || "";
  const game = body.dataset.game || "";
  let keyPair = null;
  let actorKey = "";
  let credential = "";
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
    if (!response.ok) throw new Error((await response.text()).trim() || "request failed");
    return response.json();
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
      await post("/v1/live/motion", {credential, game, actor_key: actorKey, phase, from, to});
    } catch (error) {
      const status = document.getElementById("live-status");
      if (status) status.textContent = `Motion preview refused: ${error.message}`;
    }
  }

  const board = document.querySelector(".board");
  const help = document.getElementById("move-help");
  if (board && game && help) {
    const squares = new Map();
    board.querySelectorAll(".square").forEach((square) => {
      squares.set(square.dataset.square, square);
      square.addEventListener("click", async () => {
        const selectedDestination = destinations.find((destination) => destination.slice(0, 2) === square.dataset.square);
        if (selectedFrom && selectedDestination) {
          const from = selectedFrom;
          clearDestinations();
          help.textContent = "This is a live preview. The board moves only after a durable signed move lands.";
          await publishMotion("submitting", from, selectedDestination);
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
            window.location.reload();
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
            ? `Valid moves from ${source}: ${destinations.join(", ")}. Select a destination to preview it live.`
            : `No valid move is available from ${source}${result.reason ? `: ${result.reason}.` : "."}`;
        } catch (_error) {
          help.textContent = "The durable position could not be checked.";
        }
      });
    });
  }

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
      const actor = document.createElement("code");
      actor.textContent = participant.actor.slice(0, 12);
      actor.title = participant.actor;
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
      const actor = document.createElement("code");
      actor.textContent = message.actor.slice(0, 10);
      actor.title = message.actor;
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
      if (help) help.textContent = motion.phase === "dragged"
        ? `${motion.role} is dragging from ${motion.from}; the durable board is unchanged.`
        : `${motion.role} is submitting ${motion.from} to ${motion.to}; the durable board is unchanged.`;
    });
  }

  async function observeLive() {
    if (!game) return;
    for (;;) {
      const initial = !cursor.live.generation;
      try {
        const result = await post("/v1/live/observe", {credential, game, cursor, wait_ms: initial ? 1 : 20000});
        if (result.cursor.durable.head && result.cursor.durable.head !== durableHead) {
          window.location.reload();
          return;
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

  const join = document.getElementById("join-live");
  const status = document.getElementById("live-status");
  const chatForm = document.getElementById("chat-form");
  const chatInput = document.getElementById("chat-text");
  if (join && game) {
    join.addEventListener("click", async () => {
      join.disabled = true;
      if (status) status.textContent = "Creating a temporary signing key in this tab…";
      try {
        keyPair = await window.crypto.subtle.generateKey({name: "Ed25519"}, false, ["sign", "verify"]);
        actorKey = bytesToBase64(await window.crypto.subtle.exportKey("raw", keyPair.publicKey));
        const prepared = await post("/v1/live/session/prepare", {game, actor_key: actorKey});
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
        keyPair = null;
        actorKey = "";
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
      const renewed = await post("/v1/live/session/renew", {credential, game, actor_key: actorKey});
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
