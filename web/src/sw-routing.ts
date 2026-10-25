/**
 * Service worker routing decisions, as pure functions.
 *
 * The worker itself is thin so this can be tested without a worker scope. The
 * rule that matters: the app shell must be served from cache when the PC is
 * off, because that is the only way the Wake button can exist at all — but no
 * other API response may ever be cached, or the app would show stale session
 * and file state as though it were live (docs/05-UI.md, PWA details).
 */

/** The one API response that may be cached. */
export const CACHEABLE_API_PATH = '/api/wake-config';

/** How long to wait for the network on a navigation before using the cache. */
export const NAVIGATION_TIMEOUT_MS = 4000;

export type Strategy =
  /** Try the network briefly, fall back to the cached shell. */
  | 'navigation'
  /** Serve from cache, refresh in the background. */
  | 'stale-while-revalidate'
  /** Cache first; these filenames contain a content hash. */
  | 'cache-first'
  /** Never touch the cache. */
  | 'network-only';

export interface RouteRequest {
  /** The request URL. */
  url: string;
  /** The HTTP method. */
  method: string;
  /** True when the browser is navigating, rather than fetching a subresource. */
  isNavigation: boolean;
  /** The worker's own origin, so cross-origin requests can be left alone. */
  origin: string;
}

/**
 * Pick the strategy for a request.
 */
export function strategyFor(req: RouteRequest): Strategy {
  // Anything that is not a plain GET goes straight to the network: a POST is
  // never idempotent and must not be replayed from a cache.
  if (req.method !== 'GET') return 'network-only';

  let parsed: URL;
  try {
    parsed = new URL(req.url);
  } catch {
    return 'network-only';
  }

  // Cross-origin (the waker, when one is configured) is not ours to cache.
  if (parsed.origin !== req.origin) return 'network-only';

  const path = parsed.pathname;

  if (path === CACHEABLE_API_PATH) return 'stale-while-revalidate';

  // Every other API path, including /healthz. Caching /healthz would be
  // actively harmful: the app would believe the PC is up because it was up
  // last time.
  if (path.startsWith('/api/') || path === '/healthz') return 'network-only';

  // Build output carries a content hash in the filename, so it can be cached
  // indefinitely and a new build simply asks for different names.
  if (path.startsWith('/assets/')) return 'cache-first';

  if (req.isNavigation) return 'navigation';

  // The manifest, icons and the worker itself.
  return 'stale-while-revalidate';
}

/**
 * Files precached on install, so a cold start with the PC off still works.
 *
 * Only the entry points are listed. The hashed files under /assets/ are not,
 * deliberately: a build-time list of them would have to be generated and kept
 * in step with the bundle, and a stale entry would make the install step fail
 * or cache the wrong build. They are 'cache-first' instead, so the first load
 * while the PC is on fills them in and every later load — including with the
 * PC off — is served from there.
 */
export function precacheList(): string[] {
  return ['/', '/manifest.webmanifest', CACHEABLE_API_PATH];
}

/**
 * Decide whether a response is worth storing.
 *
 * Only a complete, successful, same-origin response is: an opaque cross-origin
 * response or a partial 206 would poison the cache with something that cannot
 * be served correctly later.
 */
export function isCacheable(status: number, type: string): boolean {
  if (type === 'opaque' || type === 'error') return false;
  return status === 200;
}

/**
 * Decide whether a navigation that did get an answer should still be served
 * the cached app shell.
 *
 * During lockdown the identity gate answers 404 to everything except
 * /healthz — including the page itself. Passing that through gives a blank
 * page on a fresh launch from the home screen, and the Lockdown screen that
 * explains what happened is never seen. The shell is static and already on
 * the device, so serving it reveals nothing; the app then reads /healthz and
 * the 404 from /api/auth/state and shows the lockdown explanation.
 */
export function serveShellInstead(status: number): boolean {
  return status === 404;
}

/** Cache names are versioned so an update evicts the previous build cleanly. */
export function cacheName(version: string): string {
  return `burrow-shell-${version}`;
}

/** Caches to delete on activate: everything of ours that is not current. */
export function stalecaches(existing: string[], current: string): string[] {
  return existing.filter((n) => n.startsWith('burrow-shell-') && n !== current);
}
