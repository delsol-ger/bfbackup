// Service worker for the installed app. Network first, so an update of the
// controller shows at once; the cached shell is only used when the controller
// cannot be reached (the app then says so). The API is never cached.
const CACHE = 'bfb-shell-v2';
const SHELL = ['/', '/app.css', '/app.js', '/theme.js', '/favicon.svg', '/site.webmanifest', '/icon-192.png'];

self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', e => {
  e.waitUntil(caches.keys()
    .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.startsWith('/api/')) return;
  e.respondWith(fetch(e.request).then(resp => {
    if (resp.ok && SHELL.includes(url.pathname)) {
      const copy = resp.clone();
      caches.open(CACHE).then(c => c.put(url.pathname, copy));
    }
    return resp;
  }).catch(() => caches.match(e.request.mode === 'navigate' ? '/' : url.pathname)));
});
