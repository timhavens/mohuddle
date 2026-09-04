import { APIError, RemoteAPI } from "/api.js";
import { createDeviceIdentity, signChallenge } from "/identity.js";
import { clearDevice, loadCursor, loadDevice, saveCursor, saveDevice } from "/storage.js";
import { isTranscriptAtBottom, normalizeExcerpt, replySourceFor, transcriptRenderState } from "/timeline.mjs";

const api = new RemoteAPI();
const elements = Object.fromEntries([
  "connection-pill", "connection-label", "pair-view", "pair-form", "device-name",
  "pair-code", "pair-button", "pair-error", "room-view", "room-title", "scope-badge",
  "identity-line", "gap-banner", "gap-detail", "revoked-banner", "transcript",
  "empty-state", "composer", "message-input", "send-button", "composer-hint",
  "stop-button", "forget-button", "sync-label", "session-label", "toast",
  "admin-controls", "workflow-status", "pending-plan-controls", "pending-plan-content",
  "implement-plan-button", "decline-plan-button", "toggle-plan-button", "continue-button",
	"language-button", "decision-panel", "decision-title", "decision-question", "decision-choices",
	"decision-custom-form", "decision-custom-input", "decision-observe-note", "participant-settings-list",
	"conversation-center", "unseen-button",
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
  unseen: 0,
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

function notifyConversation(job) {
	if (!job || job.state !== "answered") return;
	const source = sourceMessage(job.source_sequence);
	const question = source?.text || "Room question";
	const title = "Chat answer ready";
	toast(`${title}: ${question.slice(0, 100)}`);
	if (navigator.vibrate) navigator.vibrate(80);
	if (document.hidden && "Notification" in window && Notification.permission === "granted") {
		new Notification(title, { body: question.slice(0, 160), tag: `conversation-${job.id}` });
	}
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

function canAdminister() {
  const values = scopes();
  return (values.includes("admin") || values.includes("administer")) && !state.revoked;
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
  elements["admin-controls"].hidden = !canAdminister();
  const planMode = String(state.room?.workflow_mode || "execute").toLowerCase() === "plan";
  const pendingPlan = state.room?.pending_plan || null;
	const conflict = state.room?.conflict || null;
  const workflows = Object.values(state.room?.workflows || {});
  const runningWorkflows = workflows.filter((workflow) => ["active", "queued", "waiting", "needs_attention"].includes(String(workflow?.state || "")));
  const modeText = planMode ? "Plan mode" : "Default mode";
	const conversations = Array.isArray(state.room?.conversations) ? state.room.conversations : [];
	const findingResponses = conversations.filter((job) => job.state === "finding").length;
	const queuedResponses = conversations.filter((job) => job.state === "waiting").length;
	const activeResponses = conversations.filter((job) => ["answering", "retrying"].includes(job.state)).length;
	const statusParts = [modeText];
	if (runningWorkflows.length) statusParts.push(`${runningWorkflows.length} workflow(s) active or waiting`);
	if (findingResponses + queuedResponses + activeResponses) statusParts.push(`chat: ${findingResponses} unassigned / finding an AI · ${queuedResponses} queued · ${activeResponses} active`);
	elements["workflow-status"].textContent = statusParts.join(" · ");
  elements["toggle-plan-button"].textContent = planMode ? "Return to Default mode" : "Enter Plan mode";
  elements["pending-plan-controls"].hidden = !pendingPlan;
  elements["pending-plan-content"].textContent = pendingPlan?.content || "";
  elements["toggle-plan-button"].hidden = Boolean(pendingPlan);
	const safeDecisionContinue = Boolean(conflict && !conflict.requires_human && conflict.recommended_id);
	elements["continue-button"].hidden = Boolean(pendingPlan) || Boolean(conflict && !safeDecisionContinue);
	elements["continue-button"].textContent = safeDecisionContinue ? "Use recommendation and continue" : "Continue workflow";
	const simpleLanguage = String(state.room?.response_style || "standard") === "simple";
	elements["language-button"].textContent = simpleLanguage ? "Use standard language" : "Use simple language";
	renderDecisionPanel(conflict);
	renderParticipantSettings();
  elements["message-input"].disabled = !canParticipate();
  elements["send-button"].disabled = !canParticipate();
	elements["message-input"].placeholder = canParticipate() ? "Ask or speak to the room…" : "Observe-only device";
  elements["composer-hint"].textContent = state.revoked
    ? "Pair again from the trusted terminal to reconnect."
    : canParticipate()
		? "Questions are answered read-only while work continues. Work requests require trusted confirmation. Stop cancels active and queued work."
      : "This device has observe scope; sending is disabled.";
	renderConversationCenter();
}

function renderParticipantSettings() {
	const container = elements["participant-settings-list"];
	const participants = Array.isArray(state.room?.participants) ? state.room.participants : [];
	const fragment = document.createDocumentFragment();
	for (const participant of participants) {
		const item = document.createElement("p");
		const actualModel = participant.reported_model || "unknown — provider did not report it";
		const actualEffort = participant.reported_effort || "unknown — provider did not report it";
		const requestedModel = participant.requested_model || "provider default requested";
		const requestedEffort = participant.requested_effort || "automatic requested";
		let permission = participant.configured_permission || "unknown";
		if (participant.active_permission) permission += ` configured → ${participant.active_permission} now`;
		else if (participant.last_turn_permission) permission += ` configured · last turn ${participant.last_turn_permission}`;
		item.textContent = `@${participant.participant} · ${participant.role} · ${participant.present ? "present" : "away"}\nActual: ${actualModel} · effort ${actualEffort}\nRequested: ${requestedModel} · effort ${requestedEffort} · permission ${permission}`;
		fragment.append(item);
	}
	if (!participants.length) {
		const item = document.createElement("p");
		item.className = "muted";
		item.textContent = "Participant configuration is unavailable.";
		fragment.append(item);
	}
	container.replaceChildren(fragment);
}

function renderDecisionPanel(conflict) {
	const panel = elements["decision-panel"];
	panel.hidden = !conflict;
	if (!conflict) return;
	elements["decision-title"].textContent = conflict.requires_human ? "MoHuddle needs your explicit choice" : "Choose how this workflow continues";
	elements["decision-question"].textContent = conflict.question || conflict.reason || "How should MoHuddle proceed?";
	const fragment = document.createDocumentFragment();
	for (const choice of Array.isArray(conflict.choices) ? conflict.choices : []) {
		const label = `${choice.label} — ${choice.consequence}${choice.id === conflict.recommended_id ? " (recommended)" : ""}`;
		const button = actionButton(label, "resolve-decision", { decisionId: conflict.decision_id, choiceId: choice.id }, choice.id === conflict.recommended_id ? "primary-button" : "text-button");
		button.disabled = !canAdminister();
		fragment.append(button);
	}
	elements["decision-choices"].replaceChildren(fragment);
	elements["decision-custom-form"].hidden = !canAdminister();
	elements["decision-observe-note"].hidden = canAdminister();
}

function sourceMessage(sequence) {
	return Array.from(state.messages.values()).find((message) => Number(message.sequence) === Number(sequence));
}

function actionButton(label, action, fields = {}, className = "text-button") {
	const button = document.createElement("button");
	button.type = "button";
	button.className = className;
	button.textContent = label;
	button.dataset.conversationAction = action;
	for (const [key, value] of Object.entries(fields)) {
		button.dataset[key] = String(value);
	}
	return button;
}

function renderConversationCenter() {
	const center = elements["conversation-center"];
	if (!center || !state.room) {
		return;
	}
	const fragment = document.createDocumentFragment();
	const pending = Array.isArray(state.room.pending_routes) ? state.room.pending_routes : [];
	const workflowActive = Boolean(state.room.workflow_active);
	for (const sequence of pending) {
		const source = sourceMessage(sequence);
		const card = document.createElement("article");
		card.className = "conversation-card needs-attention";
		const title = document.createElement("strong");
		title.textContent = "How should MoHuddle handle this message?";
		const text = document.createElement("p");
		text.className = "route-question-excerpt";
		text.textContent = normalizeExcerpt(source?.text) || `Message ${sequence}`;
		const controls = document.createElement("div");
		controls.className = "control-row";
		controls.append(actionButton("Chat", "route-chat", { sequence }, "primary-button"));
		if (canAdminister()) {
			controls.append(actionButton("Work", "route-work", { sequence }));
			if (workflowActive) controls.append(actionButton("Replace active work", "route-replace", { sequence }));
		}
		controls.append(actionButton("Dismiss", "route-cancel", { sequence }));
		card.append(title, text);
		const modeLabel = String(source?.workflow_mode || state.room.workflow_mode || "execute").toLowerCase() === "plan" ? "Plan mode" : "Default mode";
		const help = document.createElement("p");
		help.className = "muted";
		const workTiming = workflowActive ? "starts when its provider and workspace resource are available" : "starts when an agent is available";
		const replaceHelp = workflowActive ? " Replace active work targets the workflow selected by the trusted host." : "";
		help.textContent = `Chat answers read-only without starting a workflow. Work uses ${modeLabel} and ${workTiming}.${replaceHelp} Dismiss keeps the message in history.`;
		card.append(help);
		if (!canAdminister()) {
			const note = document.createElement("p");
			note.className = "muted";
			note.textContent = "Work choices require a trusted phone admin or the desktop.";
			card.append(note);
		}
		card.append(controls);
		fragment.append(card);
	}
	center.replaceChildren(fragment);
	center.hidden = !pending.length;
}

async function handleConversationAction(event) {
	const button = event.target.closest("button[data-conversation-action]");
	if (!button || button.disabled) {
		return;
	}
	const action = button.dataset.conversationAction;
	const sequence = Number(button.dataset.sequence) || 0;
	let payload;
	switch (action) {
	case "route-chat": payload = { command: "routing.resolve", sequence, intent: "conversation" }; break;
	case "route-work": payload = { command: "routing.resolve", sequence, intent: "work" }; break;
	case "route-replace":
		if (!window.confirm("Stop the active workflow and use this message instead?")) return;
		payload = { command: "routing.resolve", sequence, intent: "work", replace: true };
		break;
	case "route-cancel": payload = { command: "routing.cancel", sequence }; break;
	default: return;
	}
	button.disabled = true;
	try {
		await api.request("command.invoke", payload, roomID());
		await refreshRoomState();
	} catch (error) {
		toast(`Action was not confirmed: ${friendlyError(error)}`);
	} finally {
		button.disabled = false;
	}
}

async function refreshRoomState(preserveScroll = false) {
  const previousScroll = window.scrollY;
  const wasAtBottom = transcriptAtBottom();
  state.room = await api.request("room.get", {}, roomID());
  showRoomView();
  if (preserveScroll && !wasAtBottom) {
    requestAnimationFrame(() => window.scrollTo({ top: previousScroll, behavior: "auto" }));
  } else if (wasAtBottom) {
    requestAnimationFrame(() => followLatest());
  }
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
  const detail = error?.message || "The request could not be completed.";
  if (String(state.room?.response_style || "standard") === "simple" && error?.message) {
    return `Something went wrong. Technical detail: ${detail}`;
  }
  return detail;
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
  const additions = (history?.messages || []).filter((message) => message?.id && !state.messages.has(message.id)).length;
  const renderState = transcriptRenderState(pageScrollMetrics(), state.unseen, additions);
  let changed = false;
  for (const message of history?.messages || []) {
    changed = mergeMessage(message) || changed;
  }
  if (changed) {
    state.unseen = renderState.unseen;
    renderMessages(renderState);
    updateUnseenIndicator();
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

function pageScrollMetrics() {
  return {
    scrollHeight: document.documentElement.scrollHeight,
    scrollY: window.scrollY,
    innerHeight: window.innerHeight,
  };
}

function transcriptAtBottom() {
  return state.messages.size === 0 || isTranscriptAtBottom(pageScrollMetrics());
}

function updateUnseenIndicator() {
  const count = Math.max(0, Number(state.unseen) || 0);
  elements["unseen-button"].hidden = count === 0;
  elements["unseen-button"].textContent = `${count} new · Jump to latest`;
}

function followLatest(behavior = "auto") {
  window.scrollTo({ top: document.documentElement.scrollHeight, behavior });
  state.unseen = 0;
  updateUnseenIndicator();
}

function renderMessages({ preserveScroll = false, follow = transcriptAtBottom() } = {}) {
  const previousScroll = window.scrollY;
  const fragment = document.createDocumentFragment();
  const messages = Array.from(state.messages.values()).sort((left, right) => left.sequence - right.sequence);
  for (let messageIndex = 0; messageIndex < messages.length; messageIndex += 1) {
    const message = messages[messageIndex];
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

    item.append(heading);
    const replySource = replySourceFor(messages, messageIndex);
    if (replySource) {
      const context = document.createElement("div");
      context.className = "message-reply-context";
      const contextLabel = document.createElement("span");
      contextLabel.className = "message-reply-label";
      contextLabel.textContent = "In reply to";
      const excerpt = document.createElement("p");
      excerpt.className = "message-reply-excerpt";
      excerpt.textContent = normalizeExcerpt(replySource.text);
      context.append(contextLabel, excerpt);
      item.append(context);
    }

    const text = document.createElement("pre");
    text.className = "message-text";
    text.textContent = message.text || (message.attachments?.length ? "Attachment" : "");
    item.append(text);

    if (message.author === "user" && message.input_intent) {
      const route = document.createElement("p");
      route.className = `message-route message-route-${message.input_intent}`;
      if (message.input_intent === "work") {
		const modeLabel = String(message.workflow_mode || "execute").toLowerCase() === "plan" ? "Plan mode" : "Default mode";
		route.textContent = `Work — handled in ${modeLabel}; scheduled by provider capacity and workspace resource`;
      } else if (message.input_intent === "conversation") {
		route.textContent = "Chat — answered read-only; no workflow started";
      } else if (message.input_intent === "ambiguous") {
		route.textContent = state.room?.workflow_active
		  ? "Needs routing — choose Chat, Work, Replace active work, or Dismiss"
		  : "Needs routing — choose Chat, Work, or Dismiss";
      }
      if (route.textContent) item.append(route);
    }

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
  if (preserveScroll) {
    requestAnimationFrame(() => window.scrollTo({ top: previousScroll, behavior: "auto" }));
  } else if (follow) {
    requestAnimationFrame(() => followLatest("smooth"));
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
	  const message = frame.event.payload.message;
	  const addition = message?.id && !state.messages.has(message.id) ? 1 : 0;
	  const renderState = transcriptRenderState(pageScrollMetrics(), state.unseen, addition);
	  mergeMessage(message);
	  state.unseen = renderState.unseen;
	  renderMessages(renderState);
	  updateUnseenIndicator();
      void persistCursor();
    }
    if (frame.event?.payload?.type === "queue_changed") {
      const queued = Number(frame.event.payload.queued) || 0;
      toast(queued ? `${queued} message${queued === 1 ? "" : "s"} queued in the room.` : "The room input queue is clear.");
    }
	if (["routing_started", "wave_started", "plan_ready", "round_done", "workflow_idle", "queue_changed", "conversation"].includes(frame.event?.payload?.type)) {
	  if (frame.event?.payload?.type === "conversation") {
		notifyConversation(frame.event.payload.conversation);
	  }
      void refreshRoomState(frame.event?.payload?.type === "conversation").catch(() => {});
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
		toast("Message accepted by the room.");
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

async function invokeAdmin(command, confirmation, payload = {}) {
  if (!canAdminister() || !window.confirm(confirmation)) {
    return;
  }
  try {
    await api.request("command.invoke", { command, ...payload }, roomID());
    await refreshRoomState();
    toast("Workflow control accepted.");
  } catch (error) {
    if (isRevocation(error)) {
      markRevoked();
    } else {
      toast(`Control was not confirmed: ${friendlyError(error)}`);
    }
  }
}

async function implementPendingPlan() {
  const plan = state.room?.pending_plan;
  if (!plan) {
    return;
  }
  await invokeAdmin(
    "plan.execute",
    "Implement this exact persisted plan in a fresh Default-mode workflow? This starts writable work on the trusted host.",
    { plan_id: plan.id },
  );
}

async function declinePendingPlan() {
  const plan = state.room?.pending_plan;
  if (!plan) {
    return;
  }
  await invokeAdmin("plan.decline", "Stay in Plan mode and continue revising this plan?", { plan_id: plan.id });
}

async function togglePlanMode() {
  const planMode = String(state.room?.workflow_mode || "execute").toLowerCase() === "plan";
  await invokeAdmin(
    planMode ? "plan.off" : "plan.on",
    planMode ? "Return future requests to Default mode?" : "Put future requests into read-only Plan mode?",
  );
}

async function toggleLanguageMode() {
	const simple = String(state.room?.response_style || "standard") === "simple";
	await invokeAdmin(
		simple ? "language.standard" : "language.simple",
		simple ? "Return this room to standard response language?" : "Ask every AI and MoHuddle notice to use simpler language when possible?",
	);
}

async function resolveDecision(decisionID, choiceID = "", text = "") {
	if (!canAdminister() || !decisionID) return;
	try {
		await api.request("command.invoke", {
			command: "conflict.resolve", decision_id: decisionID, choice_id: choiceID, text,
		}, roomID());
		elements["decision-custom-input"].value = "";
		await refreshRoomState();
		toast("Decision saved; the original workflow is resuming.");
	} catch (error) {
		if (isRevocation(error)) markRevoked();
		else toast(`Decision was not confirmed: ${friendlyError(error)}`);
	}
}

function handleDecisionAction(event) {
	const button = event.target.closest("button[data-conversation-action='resolve-decision']");
	if (!button || button.disabled) return;
	const choice = Array.from(state.room?.conflict?.choices || []).find((item) => item.id === button.dataset.choiceId);
	const label = choice?.label || "this choice";
	if (window.confirm(`Use “${label}” as the binding direction and resume this workflow?`)) {
		void resolveDecision(button.dataset.decisionId || "", button.dataset.choiceId || "");
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
  state.unseen = 0;
  api.csrf = "";
  renderMessages();
  updateUnseenIndicator();
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
elements["implement-plan-button"].addEventListener("click", () => void implementPendingPlan());
elements["decline-plan-button"].addEventListener("click", () => void declinePendingPlan());
elements["toggle-plan-button"].addEventListener("click", () => void togglePlanMode());
elements["continue-button"].addEventListener("click", () => {
	const conflict = state.room?.conflict;
	const question = conflict ? "Apply the displayed recommendation and continue this workflow?" : "Continue the current workflow?";
	void invokeAdmin("continue", question);
});
elements["language-button"].addEventListener("click", () => void toggleLanguageMode());
elements["decision-choices"].addEventListener("click", handleDecisionAction);
elements["decision-custom-form"].addEventListener("submit", (event) => {
	event.preventDefault();
	const conflict = state.room?.conflict;
	const direction = elements["decision-custom-input"].value.trim();
	if (conflict?.decision_id && direction) void resolveDecision(conflict.decision_id, "", direction);
});
elements["conversation-center"].addEventListener("click", (event) => void handleConversationAction(event));
elements["forget-button"].addEventListener("click", () => void forgetPairedDevice());
elements["unseen-button"].addEventListener("click", () => followLatest("smooth"));
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
window.addEventListener("scroll", () => {
  if (state.unseen > 0 && transcriptAtBottom()) {
    state.unseen = 0;
    updateUnseenIndicator();
  }
}, { passive: true });
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

window.setInterval(() => {
	if (!elements["conversation-center"].hidden) {
		renderConversationCenter();
	}
}, 1000);

void boot();
