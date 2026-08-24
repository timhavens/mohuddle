const CACHE = "mohuddle-remote-shell-v4";
const SHELL = Object.freeze([
  "/",
  "/index.html",
  "/styles.css",
  "/app.js",
  "/api.js",
  "/identity.js",
  "/storage.js",
  "/manifest.webmanifest",
  "/icon.svg",
  "/maskable.svg",
]);
const SHELL_PATHS = new Set(SHELL);

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(SHELL)));
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  if (event.request.method !== "GET") {
    return;
  }
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith("/api/") || !SHELL_PATHS.has(url.pathname)) {
    return;
  }
  event.respondWith(caches.match(event.request).then((cached) => cached || fetch(event.request)));
});
