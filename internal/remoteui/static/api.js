const VERSION = "mohuddle.v1";

export class APIError extends Error {
  constructor(message, code = "request_failed", status = 0) {
    super(message);
    this.name = "APIError";
    this.code = code;
    this.status = status;
  }
}

async function parseJSON(response) {
  let value;
  try {
    value = await response.json();
  } catch {
    throw new APIError("The gateway returned an invalid response.", "invalid_response", response.status);
  }
  if (!response.ok) {
    const detail = value?.error;
    throw new APIError(
      detail?.message || value?.message || `Request failed (${response.status}).`,
      detail?.code || value?.code || "request_failed",
      response.status,
    );
  }
  return value;
}

async function post(path, body, csrf = "") {
  const headers = { "Content-Type": "application/json", Accept: "application/json" };
  if (csrf) {
    headers["X-MoHuddle-CSRF"] = csrf;
  }
  let response;
  try {
    response = await fetch(path, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
      cache: "no-store",
      credentials: "same-origin",
      redirect: "error",
    });
  } catch (error) {
    throw new APIError(error?.message || "The gateway is unreachable.", "offline");
  }
  return parseJSON(response);
}

function requestID() {
  if (typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("");
}

export class RemoteAPI {
  constructor() {
    this.csrf = "";
  }

  pair(code, name, publicKey) {
    return post("/api/v1/pair", { code, name, public_key: publicKey });
  }

  challenge(deviceID) {
    return post("/api/v1/challenge", { device_id: deviceID });
  }

  async session(deviceID, challengeID, signature) {
    const result = await post("/api/v1/session", {
      device_id: deviceID,
      challenge_id: challengeID,
      signature,
    });
    this.csrf = result.csrf_token || result.csrf || "";
    if (!this.csrf) {
      throw new APIError("The gateway did not issue CSRF protection.", "invalid_response");
    }
    return result;
  }

  async request(type, payload = {}, roomID = "") {
    if (!this.csrf) {
      throw new APIError("The remote session has expired.", "session_expired", 401);
    }
    const request = {
      version: VERSION,
      id: requestID(),
      type,
      payload,
    };
    if (roomID) {
      request.room_id = roomID;
    }
    const response = await post("/api/v1/request", request, this.csrf);
    if (response?.ok === false) {
      throw new APIError(
        response.error?.message || "MoHuddle rejected the request.",
        response.error?.code || "request_failed",
      );
    }
    return Object.hasOwn(response || {}, "result") ? response.result : response;
  }

  events(cursor, onFrame) {
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const url = new URL(`${scheme}//${location.host}/api/v1/events`);
    url.searchParams.set("room_id", cursor.room_id || "");
    if (cursor.boot_id) {
      url.searchParams.set("boot_id", cursor.boot_id);
    }
    url.searchParams.set("after_event", String(cursor.event_sequence || 0));
    url.searchParams.set("after_message", String(cursor.message_sequence || 0));

    const socket = new WebSocket(url);
    return new Promise((resolve, reject) => {
      let opened = false;
      socket.addEventListener("open", () => {
        opened = true;
        resolve(socket);
      }, { once: true });
      socket.addEventListener("message", (event) => {
        try {
          onFrame(JSON.parse(event.data));
        } catch {
          onFrame({ type: "gap", gap: { reason: "invalid event frame" } });
        }
      });
      socket.addEventListener("error", () => {
        if (!opened) {
          reject(new APIError("Could not open the room event stream.", "offline"));
        }
      }, { once: true });
      socket.addEventListener("close", (event) => {
        if (!opened) {
		  const code = event.code === 4003 ? "revoked" : event.code === 4001 ? "session_expired" : "stream_closed";
		  reject(new APIError("The room event stream was rejected.", code));
        }
      }, { once: true });
    });
  }
}
