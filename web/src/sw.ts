/// <reference lib="webworker" />
/**
 * Burrow's service worker.
 *
 * Hand-written rather than generated, because its one hard requirement is
 * unusual: the app has to open and show a Wake button while the server that
 * serves it is powered off. That means the shell and /api/wake-config are
 * cached, and nothing else is.
 */

import {
  CACHEABLE_API_PATH,
  NAVIGATION_TIMEOUT_MS,
  cacheName,
  isCacheable,
  precacheList,
  serveShellInstead,
  stalecaches,
  strategyFor,
} from './sw-routing';

declare const self: ServiceWorkerGlobalScope;

// Replaced at build time by Vite's define, so each build gets its own cache
// and activating a new worker evicts the previous build's files.
const CACHE = cacheName(__BURROW_VERSION__);

self.addEventListener('install', (event) => {
  event.waitUntil(
    (async () => {
      const cache = await self.caches.open(CACHE);
      // Each entry is added individually: addAll rejects the whole batch if
      // any one request fails, and a missing icon should not stop the shell
      // being cached.
      await Promise.all(
        precacheList().map(async (url) => {
          try {
            const res = await fetch(url, { cache: 'reload' });
            if (isCacheable(res.status, res.type)) await cache.put(url, res);
          } catch {
            // Offline at install time: the runtime handlers will fill it in.
          }
        }),
      );
      // Take over at once, so a fresh install can serve the shell on the very
      // next navigation rather than the one after.
      await self.skipWaiting();
    })(),
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      const names = await self.caches.keys();
      await Promise.all(stalecaches(names, CACHE).map((n) => self.caches.delete(n)));
      await self.clients.claim();
    })(),
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  const strategy = strategyFor({
    url: req.url,
    method: req.method,
    isNavigation: req.mode === 'navigate',
    origin: self.location.origin,
  });

  switch (strategy) {
    case 'network-only':
      return; // let the browser handle it
    case 'navigation':
      event.respondWith(handleNavigation(req));
      return;
    case 'cache-first':
      event.respondWith(cacheFirst(req));
      return;
    case 'stale-while-revalidate':
      event.respondWith(staleWhileRevalidate(req));
      return;
  }
});

/**
 * Navigations: race the network against a short timer.
 *
 * Without the timer, opening the app with the PC off would hang on a TCP
 * connection that never completes, which is exactly the moment the user needs
 * the Wake button.
 */
async function handleNavigation(req: Request): Promise<Response> {
  const cache = await self.caches.open(CACHE);

  try {
    const res = await fetchWithTimeout(req, NAVIGATION_TIMEOUT_MS);
    if (isCacheable(res.status, res.type)) {
      await cache.put('/', res.clone());
    } else if (serveShellInstead(res.status)) {
      const shell = await cache.match('/');
      if (shell) return shell;
    }
    return res;
  } catch {
    const cached = (await cache.match('/')) ?? (await cache.match(req));
    if (cached) return cached;
    // Nothing cached yet: a plain, honest page rather than the browser's
    // network error, which gives the user nothing to act on.
    return new Response(offlineFallbackHTML, {
      status: 200,
      headers: { 'Content-Type': 'text/html; charset=utf-8' },
    });
  }
}

async function cacheFirst(req: Request): Promise<Response> {
  const cache = await self.caches.open(CACHE);
  const cached = await cache.match(req);
  if (cached) return cached;

  const res = await fetch(req);
  if (isCacheable(res.status, res.type)) await cache.put(req, res.clone());
  return res;
}

async function staleWhileRevalidate(req: Request): Promise<Response> {
  const cache = await self.caches.open(CACHE);
  const cached = await cache.match(req);

  const refresh = (async () => {
    try {
      const res = await fetchWithTimeout(req, NAVIGATION_TIMEOUT_MS);
      if (isCacheable(res.status, res.type)) await cache.put(req, res.clone());
      return res;
    } catch {
      return undefined;
    }
  })();

  if (cached) {
    // Served from cache now; the refresh completes in the background.
    void refresh;
    return cached;
  }
  const fresh = await refresh;
  if (fresh) return fresh;

  if (new URL(req.url).pathname === CACHEABLE_API_PATH) {
    // Without a wake config the app cannot offer Wake at all, so a usable
    // default is better than a failure. These match config/burrow.example.yaml.
    return new Response(
      JSON.stringify({
        method: 'smartplug',
        shortcuts: { on: 'PC On', off: 'PC Off', powerCycle: 'PC Power Cycle' },
        wakeTimeoutSec: 180,
        powerCycleAfterSec: 90,
        offConfirmSec: 30,
        wakerUrl: null,
        routerHint: '',
      }),
      { status: 200, headers: { 'Content-Type': 'application/json; charset=utf-8' } },
    );
  }
  return new Response('', { status: 504 });
}

/** fetch with an abort after ms, so a dead server does not hang a response. */
async function fetchWithTimeout(req: Request, ms: number): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), ms);
  try {
    return await fetch(req, { signal: controller.signal });
  } finally {
    clearTimeout(timer);
  }
}

/**
 * The last-resort page, used only before the shell has ever been cached.
 *
 * It carries no script: at this point the app's assets are not available
 * either, and a reload is the only useful action.
 */
const offlineFallbackHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Burrow</title>
<style>
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         background:#14161a; color:#e8eaed;
         font-family:-apple-system, system-ui, sans-serif; text-align:center; padding:24px; }
  p { color:#9aa0a6; max-width:28em; line-height:1.5; }
</style>
</head><body><main>
<h1>Burrow</h1>
<p>The PC is not reachable, and the app has not been saved for offline use yet.
Open Burrow once while the PC is on, then it will work from here even when the
PC is off.</p>
</main></body></html>`;
