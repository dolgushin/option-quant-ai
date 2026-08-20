const CACHE = 'optionquant-v1';
const CORE = [
  '/',
  '/manifest.json',
  '/icon.svg',
  '/icon-512.png',
  '/vendor/chart.umd.min.js',
  'https://cdn.tailwindcss.com'
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE).then((c) => c.addAll(CORE)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

// Stale-while-revalidate: serve cached HTML/assets, refresh in background.
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET') return;
  if (url.origin !== self.location.origin && !url.hostname.endsWith('cdn.jsdelivr.net') && url.hostname !== 'cdn.tailwindcss.com') return;

  e.respondWith(
    caches.open(CACHE).then(async (cache) => {
      const cached = await cache.match(e.request, { ignoreSearch: url.pathname === '/' });
      const network = fetch(e.request)
        .then((resp) => {
          if (resp && resp.status === 200 && (resp.type === 'basic' || resp.type === 'cors')) {
            cache.put(e.request, resp.clone());
          }
          return resp;
        })
        .catch(() => cached);
      return cached || network;
    })
  );
});