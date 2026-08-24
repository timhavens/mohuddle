import { APIError, RemoteAPI } from "/api.js";
import { createDeviceIdentity, signChallenge } from "/identity.js";
import { clearDevice, loadCursor, loadDevice, saveCursor, saveDevice } from "/storage.js";

const api = new RemoteAPI();
const elements = Object.fromEntries([
  "connection-pill", "connection-label", "pair-view", "pair-form", "device-name",
  "pair-code", "pair-button", "pair-error", "room-view", "room-title", "scope-badge",
  "identity-line", "gap-banner", "gap-detail", "revoked-banner", "transcript",
  "empty-state", "composer", "message-input", "send-button", "composer-hint",
  "stop-button", "forget-button", "sync-label", "session-label", "toast",
].map((id) => [id, document.getElementById(id)]));

const state = {
  device: null,
  session: null,
  room: null,
  messages: new Map(),
  cursor: { boot_id: "", event_sequence: 0, message_sequence: 0 },
  socket: null,
  reconnectTimer: 0,
  reconnectAttempt: 0,
  sessionTimer: 0,
  toastTimer: 0,
  intentionalClose: false,
  connecting: false,
  recovering: false,
  revoked: false,
};

function pairingCodeFromFragment() {
  const fragment = location.hash.replace(/^#/, "");
  if (!fragment) {
    return "";
  }
  const parameters = new URLSearchParams(fragment);
  const code = parameters.get("code") || (fragment.includes("=") ? "" : decodeURIComponent(fragment));
  history.replaceState(null, "", `${location.pathname}${location.search}`);
  return code.trim();
}

function setConnection(kind, label) {
  elements["connection-pill"].className = `pill pill-${kind}`;
  elements["connection-label"].textContent = label;
}

function setPairError(message = "") {
  elements["pair-error"].textContent = message;
  elements["pair-error"].hidden = !message;
}

function setPairBusy(busy) {
  elements["pair-button"].disabled = busy;
  elements["device-name"].disabled = busy;
  elements["pair-code"].disabled = busy;
  elements["pair-button"].textContent = busy ? "Pairing…" : "Pair device";
}

function toast(message) {
  clearTimeout(state.toastTimer);
  elements.toast.textContent = message;
  elements.toast.hidden = false;
  state.toastTimer = window.setTimeout(() => {
    elements.toast.hidden = true;
  }, 3600);
}

function showPairView() {
  elements["pair-view"].hidden = false;
  elements["room-view"].hidden = true;
  elements["session-label"].textContent = "No session";
  setConnection(navigator.onLine ? "offline" : "offline", navigator.onLine ? "Not paired" : "Offline");
}

function roomID() {
  return state.session?.room_id || state.session?.room || state.device?.room_id || state.device?.room || "";
}

function scopes() {
  const values = state.session?.scopes || state.device?.scopes || [];
  return Array.isArray(values) ? values.map((value) => String(value).toLowerCase()) : [];
}

function canParticipate() {
  return scopes().includes("participate") && !state.revoked;
}

function showRoomView() {
  elements["pair-view"].hidden = true;
  elements["room-view"].hidden = false;
  const id = state.room?.id || roomID();
  elements["room-title"].textContent = id ? `Room ${shortID(id)}` : "Remote room";
  const effectiveScope = scopes().includes("admin") || scopes().includes("administer")
    ? "admin"
    : canParticipate() ? "participate" : "observe";
  elements["scope-badge"].textContent = effectiveScope;
  const identity = state.session?.identity || state.device?.device_id || "paired device";
  elements["identity-line"].textContent = `${identity} · ${id || "room unavailable"}`;
  elements["revoked-banner"].hidden = !state.revoked;
  elements.composer.hidden = state.revoked;
  elements["stop-button"].hidden = !canParticipate();
  elements["stop-button"].disabled = state.revoked;
  elements["message-input"].disabled = !canParticipate();
  elements["send-button"].disabled = !canParticipate();
  elements["message-input"].placeholder = canParticipate() ? "Read-only ask…" : "Observe-only device";
  elements["composer-hint"].textContent = state.revoked
    ? "Pair again from the trusted terminal to reconnect."
    : canParticipate()
      ? "Messages are isolated read-only asks. Use Stop all work, or type /stop, to cancel active and queued work."
      : "This device has observe scope; sending is disabled.";
}

function shortID(value) {
  const text = String(value || "");
  return text.length > 12 ? `${text.slice(0, 8)}…${text.slice(-4)}` : text;
}

function friendlyError(error) {
  if (!navigator.onLine || error?.code === "offline") {
    return "The gateway is unreachable. Check this device's connection and the configured tunnel.";
  }
  if (error?.code === "revoked") {
    return "This device has been revoked by the trusted host.";
  }
  if (error?.code === "session_expired" || error?.status === 401) {
    return "The remote session expired and could not be renewed.";
  }
  return error?.message || "The request could not be completed.";
}

function isRevocation(error) {
  return error?.code === "revoked" || error?.code === "device_revoked" || error?.status === 403;
}

function normalizeSession(value) {
  return {
    ...value,
    device_id: value.device_id || value.device || state.device?.device_id || "",
    room_id: value.room_id || value.room || state.device?.room_id || "",
    csrf_token: value.csrf_token || value.csrf || "",
    expires_at: value.expires_at || value.expires || "",
  };
}

function normalizeCursor(value = {}) {
  return {
    boot_id: String(value.boot_id || ""),
    event_sequence: Math.max(0, Number(value.event_sequence) || 0),
    message_sequence: Math.max(0, Number(value.message_sequence) || 0),
  };
}

async function persistCursor() {
  const id = state.device?.device_id;
  const room = roomID();
  if (!id || !room) {
    return;
  }
  try {
    await saveCursor(id, room, state.cursor);
  } catch {
    // Cursor persistence is an optimization. A later connection can recover
    // from sequence zero without weakening authentication or losing history.
  }
}

function applyCursor(value) {
  if (!value) {
    return;
  }
  const next = normalizeCursor(value);
  if (state.cursor.boot_id && next.boot_id && state.cursor.boot_id !== next.boot_id) {
    state.cursor.event_sequence = 0;
  }
  state.cursor = next;
  void persistCursor();
  elements["sync-label"].textContent = `Message ${state.cursor.message_sequence} · event ${state.cursor.event_sequence}`;
}

function mergeMessage(message) {
  if (!message || !message.id || !Number.isFinite(Number(message.sequence))) {
    return false;
  }
  const sequence = Number(message.sequence);
  const previous = state.messages.get(message.id);
  state.messages.set(message.id, { ...previous, ...message, sequence });
  state.cursor.message_sequence = Math.max(state.cursor.message_sequence, sequence);
  return true;
}

function mergeHistory(history) {
  let changed = false;
  for (const message of history?.messages || []) {
    changed = mergeMessage(message) || changed;
  }
  if (changed) {
    renderMessages();
    void persistCursor();
  }
}

function lastHistorySequence(history, fallback = 0) {
  const messages = history?.messages || [];
  if (!messages.length) {
    return fallback;
  }
  return Number(messages[messages.length - 1].sequence) || fallback;
}

function messageClass(kind, author) {
  if (String(author).toLowerCase() === "user") {
    return "message message-user";
  }
  const normalized = String(kind || "").toLowerCase();
  if (["system", "status", "tool"].includes(normalized)) {
    return `message message-${normalized}`;
  }
  return "message";
}

function renderMessages() {
  const nearBottom = document.documentElement.scrollHeight - window.scrollY - window.innerHeight < 150;
  const fragment = document.createDocumentFragment();
  const messages = Array.from(state.messages.values()).sort((left, right) => left.sequence - right.sequence);
  for (const message of messages) {
    const item = document.createElement("li");
    item.className = messageClass(message.kind, message.author);
    item.dataset.sequence = String(message.sequence);

    const heading = document.createElement("div");
    heading.className = "message-heading";
    const author = document.createElement("span");
    author.className = "message-author";
    author.textContent = message.author === "user" ? "You" : `@${message.author || "system"}`;
    const time = document.createElement("time");
    time.className = "message-time";
    time.dateTime = message.created_at || "";
    time.textContent = formatTime(message.created_at);
    heading.append(author, time);

    const text = document.createElement("pre");
    text.className = "message-text";
    text.textContent = message.text || (message.attachments?.length ? "Attachment" : "");
    item.append(heading, text);

    if (message.attachments?.length) {
      const attachments = document.createElement("div");
      attachments.className = "attachment-list";
      for (const attachment of message.attachments) {
        const label = document.createElement("span");
        label.className = "attachment";
        const size = attachment.size ? ` · ${formatBytes(attachment.size)}` : "";
        label.textContent = `${attachment.name || attachment.kind || "attachment"}${size}`;
        attachments.append(label);
      }
      item.append(attachments);
    }
    fragment.append(item);
  }
  elements.transcript.replaceChildren(fragment);
  elements["empty-state"].hidden = messages.length > 0;
  if (nearBottom) {
    requestAnimationFrame(() => window.scrollTo({ top: document.documentElement.scrollHeight, behavior: "smooth" }));
  }
}

function formatTime(value) {
  const date = new Date(value || 0);
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" }).format(date);
}

function formatBytes(value) {
  const bytes = Number(value) || 0;
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function updateSessionExpiry() {
  clearTimeout(state.sessionTimer);
  const expiry = new Date(state.session?.expires_at || 0);
  if (Number.isNaN(expiry.getTime())) {
    elements["session-label"].textContent = "Paired session";
    return;
  }
  elements["session-label"].textContent = `Session until ${formatTime(expiry)}`;
  const renewIn = Math.max(1000, expiry.getTime() - Date.now() - 30_000);
  state.sessionTimer = window.setTimeout(() => {
    if (navigator.onLine && !state.revoked) {
      void renewAndReconnect();
    }
  }, Math.min(renewIn, 2_147_000_000));
}

async function authenticate() {
  if (!state.device?.device_id || !state.device?.privateKey) {
    throw new APIError("No paired device key is available.", "not_paired");
  }
  setConnection("working", "Authenticating");
  const challenge = await api.challenge(state.device.device_id);
  if (!challenge.challenge_id || !challenge.payload) {
    throw new APIError("The gateway returned an invalid challenge.", "invalid_response");
  }
  const signature = await signChallenge(state.device.privateKey, challenge.payload);
  state.session = normalizeSession(await api.session(state.device.device_id, challenge.challenge_id, signature));
  state.revoked = false;
  state.device = {
    ...state.device,
    device_id: state.session.device_id,
    room_id: state.session.room_id,
    scopes: state.session.scopes || state.device.scopes || [],
    identity: state.session.identity || state.device.identity || "",
  };
  await saveDevice(state.device);
  await api.request("room.join", { room_id: state.session.room_id });
  updateSessionExpiry();
  showRoomView();
}

async function loadSavedCursor() {
  const saved = await loadCursor(state.device.device_id, roomID());
  if (saved) {
    state.cursor = normalizeCursor(saved);
  }
}

function streamRequestCursor() {
  // A cold page intentionally asks for durable history from zero because the
  // application does not persist transcript contents. Runtime reconnects can
  // continue from the highest message already rendered in memory.
  const hasMessages = state.messages.size > 0;
  return {
    room_id: roomID(),
    boot_id: state.cursor.boot_id,
    event_sequence: state.cursor.event_sequence,
    message_sequence: hasMessages ? state.cursor.message_sequence : 0,
  };
}

async function connectStream() {
  if (state.connecting || state.revoked || !state.session) {
    return;
  }
  state.connecting = true;
  state.intentionalClose = false;
  setConnection("working", "Synchronizing");
  try {
    const socket = await api.events(streamRequestCursor(), handleFrame);
    if (state.socket && state.socket !== socket) {
      state.intentionalClose = true;
      state.socket.close(1000, "replaced");
      state.intentionalClose = false;
    }
    state.socket = socket;
    state.reconnectAttempt = 0;
    socket.addEventListener("close", (event) => {
      if (state.socket !== socket) {
        return;
      }
      state.socket = null;
      if (state.intentionalClose) {
        return;
      }
      if (event.code === 4003 || event.code === 4403) {
        markRevoked();
        return;
      }
	  if (event.code === 4001 || event.code === 4401) {
		state.session = null;
		api.csrf = "";
		setConnection("working", "Authorization changed");
		scheduleReconnect();
		return;
	  }
      scheduleReconnect();
    });
    setConnection("online", "Connected");
  } finally {
    state.connecting = false;
  }
}

function handleFrame(frame) {
  if (!frame || typeof frame !== "object") {
    showGap({ reason: "invalid event frame" });
    return;
  }
  switch (frame.type) {
  case "sync":
	applyCursor(frame.cursor);
    if (frame.room) {
      state.room = frame.room;
      showRoomView();
    }
    mergeHistory(frame.history);
    if (frame.gap) {
      showGap(frame.gap);
	  void recoverHistory(frame.gap.history_after, frame.gap.current?.message_sequence);
    } else if (frame.history?.has_more) {
      showGap({ reason: "more room history is available" });
	  void recoverHistory(lastHistorySequence(frame.history), frame.history.through);
    } else {
      clearGap();
    }
    setConnection("online", "Connected");
    break;
  case "event":
    applyCursor(frame.cursor);
    if (frame.event?.payload?.message) {
      mergeMessage(frame.event.payload.message);
      renderMessages();
      void persistCursor();
    }
    if (frame.event?.payload?.type === "queue_changed") {
      const queued = Number(frame.event.payload.queued) || 0;
      toast(queued ? `${queued} message${queued === 1 ? "" : "s"} queued in the room.` : "The room input queue is clear.");
    }
    break;
  case "gap":
    applyCursor(frame.cursor);
    showGap(frame.gap);
	void recoverHistory(frame.gap?.history_after, frame.gap?.current?.message_sequence);
    break;
  default:
    showGap({ reason: "unsupported event frame" });
  }
}

function showGap(gap = {}) {
  elements["gap-banner"].hidden = false;
  const reason = gap.reason ? `${gap.reason}. ` : "";
  elements["gap-detail"].textContent = `${reason}Recovering durable room history…`;
}

function clearGap() {
  elements["gap-banner"].hidden = true;
  elements["gap-detail"].textContent = "Recovering durable room history…";
}

async function recoverHistory(after, through = 0) {
  if (state.recovering || !state.session || state.revoked) {
    return;
  }
  state.recovering = true;
  let cursor = Math.max(0, Number(after) || 0);
  try {
    for (let page = 0; page < 100; page += 1) {
	  const payload = { after: cursor, limit: 1000 };
	  if (Number(through) > 0) {
		payload.through = Number(through);
	  }
	  const historyResult = await api.request("history.get", payload, roomID());
      mergeHistory(historyResult);
      const messages = historyResult?.messages || [];
      if (messages.length) {
        cursor = Number(messages[messages.length - 1].sequence) || cursor;
      }
      if (!historyResult?.has_more || !messages.length) {
        break;
      }
    }
    elements["gap-detail"].textContent = "Durable transcript recovered; transient activity during the gap is unavailable.";
    window.setTimeout(clearGap, 5000);
  } catch (error) {
    elements["gap-detail"].textContent = `History recovery failed: ${friendlyError(error)}`;
  } finally {
    state.recovering = false;
  }
}

function scheduleReconnect() {
  clearTimeout(state.reconnectTimer);
  if (state.revoked) {
    return;
  }
  if (!navigator.onLine) {
    setConnection("offline", "Offline");
    return;
  }
  const delay = Math.min(30_000, 750 * (2 ** Math.min(state.reconnectAttempt, 6)));
  state.reconnectAttempt += 1;
  setConnection("working", `Reconnecting in ${Math.ceil(delay / 1000)}s`);
  state.reconnectTimer = window.setTimeout(() => void renewAndReconnect(), delay);
}

async function renewAndReconnect() {
  if (state.connecting || state.revoked || !state.device) {
    return;
  }
  try {
    await authenticate();
    await connectStream();
  } catch (error) {
    if (isRevocation(error)) {
      markRevoked();
      return;
    }
    setConnection(navigator.onLine ? "error" : "offline", navigator.onLine ? "Reconnect failed" : "Offline");
    scheduleReconnect();
  }
}

function markRevoked() {
  state.revoked = true;
  state.session = null;
  api.csrf = "";
  clearTimeout(state.reconnectTimer);
  clearTimeout(state.sessionTimer);
  setConnection("revoked", "Revoked");
  showRoomView();
}

async function startPairedDevice() {
  try {
    await authenticate();
    await loadSavedCursor();
    await connectStream();
  } catch (error) {
    if (isRevocation(error)) {
      markRevoked();
      return;
    }
    showRoomView();
    setConnection(navigator.onLine ? "error" : "offline", navigator.onLine ? "Connection failed" : "Offline");
    toast(friendlyError(error));
    scheduleReconnect();
  }
}

async function pairDevice(event) {
  event.preventDefault();
  setPairError();
  const code = elements["pair-code"].value.trim();
  const name = elements["device-name"].value.trim();
  if (!code || !name) {
    setPairError("A device name and pairing code are required.");
    return;
  }
  setPairBusy(true);
  setConnection("working", "Pairing");
  try {
    const identity = await createDeviceIdentity();
    const result = await api.pair(code, name, identity.publicKey);
    const deviceID = result.device_id || result.device;
    if (!deviceID) {
      throw new APIError("The gateway did not return a device identity.", "invalid_response");
    }
    state.device = {
      device_id: deviceID,
      name: result.name || name,
      privateKey: identity.privateKey,
      scopes: result.scopes || [],
      room_id: result.room_id || result.room || "",
    };
    await saveDevice(state.device);
    elements["pair-code"].value = "";
    await startPairedDevice();
  } catch (error) {
    setPairError(friendlyError(error));
    setConnection(navigator.onLine ? "error" : "offline", navigator.onLine ? "Pairing failed" : "Offline");
  } finally {
    setPairBusy(false);
  }
}

async function sendMessage(event) {
  event.preventDefault();
  const text = elements["message-input"].value.trim();
  if (!text || !canParticipate()) {
    return;
  }
  if (text === "/stop") {
    if (await stopAllWork()) {
      elements["message-input"].value = "";
    }
    return;
  }
  elements["send-button"].disabled = true;
  elements["message-input"].disabled = true;
  try {
    await api.request("message.send", { mode: "ask", text }, roomID());
    elements["message-input"].value = "";
    toast("Read-only ask accepted by the room.");
  } catch (error) {
    if (isRevocation(error)) {
      markRevoked();
    } else {
      toast(`Message not confirmed: ${friendlyError(error)}`);
    }
  } finally {
    elements["message-input"].disabled = !canParticipate();
    elements["send-button"].disabled = !canParticipate();
  }
}

async function stopAllWork() {
  if (!canParticipate() || !window.confirm("Stop all active work and clear every queued message in this room?")) {
    return false;
  }
  elements["stop-button"].disabled = true;
  try {
    await api.request("command.invoke", { command: "stop" }, roomID());
    toast("All active and queued work stopped.");
    return true;
  } catch (error) {
    if (isRevocation(error)) {
      markRevoked();
    } else {
      toast(`Stop was not confirmed: ${friendlyError(error)}`);
    }
    return false;
  } finally {
    elements["stop-button"].disabled = !canParticipate();
  }
}

async function forgetPairedDevice() {
  if (!window.confirm("Forget this device key and pairing from this browser? The host's revocation record is unaffected.")) {
    return;
  }
  state.intentionalClose = true;
  state.socket?.close(1000, "device forgotten");
  state.socket = null;
  clearTimeout(state.reconnectTimer);
  clearTimeout(state.sessionTimer);
  await clearDevice();
  state.device = null;
  state.session = null;
  state.room = null;
  state.messages.clear();
  state.cursor = { boot_id: "", event_sequence: 0, message_sequence: 0 };
  state.revoked = false;
  api.csrf = "";
  renderMessages();
  showPairView();
}

async function boot() {
  elements["pair-code"].value = pairingCodeFromFragment();
  elements["device-name"].value = localStorage.getItem("mohuddle-device-name") || "";
  try {
    state.device = await loadDevice();
  } catch (error) {
    showPairView();
    setPairError(`Secure device storage is unavailable: ${friendlyError(error)}`);
    return;
  }
  if (!state.device) {
    showPairView();
    if (elements["pair-code"].value) {
      elements["device-name"].focus();
    }
    return;
  }
  showRoomView();
  await startPairedDevice();
}

elements["pair-form"].addEventListener("submit", (event) => {
  localStorage.setItem("mohuddle-device-name", elements["device-name"].value.trim());
  void pairDevice(event);
});
elements.composer.addEventListener("submit", (event) => void sendMessage(event));
elements["stop-button"].addEventListener("click", () => void stopAllWork());
elements["forget-button"].addEventListener("click", () => void forgetPairedDevice());
elements["message-input"].addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    elements.composer.requestSubmit();
  }
});
window.addEventListener("offline", () => {
  setConnection("offline", "Offline");
});
window.addEventListener("online", () => {
  if (state.device && !state.socket && !state.revoked) {
    clearTimeout(state.reconnectTimer);
    void renewAndReconnect();
  }
});
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && state.device && !state.socket && navigator.onLine && !state.revoked) {
    clearTimeout(state.reconnectTimer);
    void renewAndReconnect();
  }
});

if ("serviceWorker" in navigator && window.isSecureContext) {
  navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch(() => {
    // Installation is optional; the authenticated online client still works.
  });
}

void boot();
