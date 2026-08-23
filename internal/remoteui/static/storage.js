const DATABASE = "mohuddle-remote-v1";
const VERSION = 1;
const STORE = "state";
const DEVICE_KEY = "device";

let openPromise;

function database() {
  if (!openPromise) {
    openPromise = new Promise((resolve, reject) => {
      const request = indexedDB.open(DATABASE, VERSION);
      request.onupgradeneeded = () => {
        if (!request.result.objectStoreNames.contains(STORE)) {
          request.result.createObjectStore(STORE);
        }
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error || new Error("Could not open device storage."));
      request.onblocked = () => reject(new Error("Device storage upgrade was blocked."));
    });
  }
  return openPromise;
}

async function transaction(mode, action) {
  const db = await database();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, mode);
    const store = tx.objectStore(STORE);
    let value;
    try {
      value = action(store);
    } catch (error) {
      reject(error);
      return;
    }
    tx.oncomplete = () => resolve(value);
    tx.onerror = () => reject(tx.error || new Error("Device storage transaction failed."));
    tx.onabort = () => reject(tx.error || new Error("Device storage transaction was aborted."));
  });
}

function read(key) {
  return database().then((db) => new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readonly");
    const request = tx.objectStore(STORE).get(key);
    request.onsuccess = () => resolve(request.result ?? null);
    request.onerror = () => reject(request.error || new Error("Could not read device storage."));
  }));
}

function cursorKey(deviceID, roomID) {
  return `cursor:${deviceID}:${roomID}`;
}

export function loadDevice() {
  return read(DEVICE_KEY);
}

export function saveDevice(device) {
  return transaction("readwrite", (store) => store.put(device, DEVICE_KEY));
}

export function clearDevice() {
  return transaction("readwrite", (store) => store.clear());
}

export function loadCursor(deviceID, roomID) {
  return read(cursorKey(deviceID, roomID));
}

export function saveCursor(deviceID, roomID, cursor) {
  return transaction("readwrite", (store) => store.put(cursor, cursorKey(deviceID, roomID)));
}
