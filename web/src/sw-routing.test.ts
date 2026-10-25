import { describe, expect, it } from 'vitest';
import {
  CACHEABLE_API_PATH,
  cacheName,
  isCacheable,
  precacheList,
  serveShellInstead,
  stalecaches,
  strategyFor,
  type RouteRequest,
} from './sw-routing';

const ORIGIN = 'https://n-ab12cd34.tail1234.ts.net';

function req(path: string, over: Partial<RouteRequest> = {}): RouteRequest {
  return {
    url: path.startsWith('http') ? path : ORIGIN + path,
    method: 'GET',
    isNavigation: false,
    origin: ORIGIN,
    ...over,
  };
}

describe('strategyFor', () => {
  it('serves navigations network-first with a cached fallback', () => {
    expect(strategyFor(req('/', { isNavigation: true }))).toBe('navigation');
    expect(strategyFor(req('/files', { isNavigation: true }))).toBe('navigation');
    expect(strategyFor(req('/pair', { isNavigation: true }))).toBe('navigation');
  });

  // The rule the test plan calls out: no API response is cached except the
  // wake config, because stale session or file state shown as live is worse
  // than no answer.
  it('never caches an API response other than the wake config', () => {
    for (const path of [
      '/api/system',
      '/api/sessions',
      '/api/files/list?root=Docs',
      '/api/auth/state',
      '/api/audit',
      '/api/devices',
      '/healthz',
    ]) {
      expect(strategyFor(req(path)), path).toBe('network-only');
    }
  });

  it('caches the wake config, which is what makes the offline Wake screen work', () => {
    expect(strategyFor(req(CACHEABLE_API_PATH))).toBe('stale-while-revalidate');
  });

  it('caches hashed build assets indefinitely', () => {
    expect(strategyFor(req('/assets/index-a1b2c3.js'))).toBe('cache-first');
    expect(strategyFor(req('/assets/index-a1b2c3.css'))).toBe('cache-first');
  });

  it('revalidates the shell files that have stable names', () => {
    expect(strategyFor(req('/manifest.webmanifest'))).toBe('stale-while-revalidate');
    expect(strategyFor(req('/icons/icon-192.png'))).toBe('stale-while-revalidate');
    expect(strategyFor(req('/sw.js'))).toBe('stale-while-revalidate');
  });

  it('never caches a non-GET request', () => {
    for (const method of ['POST', 'PUT', 'PATCH', 'DELETE', 'HEAD']) {
      expect(strategyFor(req('/', { method, isNavigation: true })), method).toBe('network-only');
      expect(strategyFor(req(CACHEABLE_API_PATH, { method })), method).toBe('network-only');
    }
  });

  it('leaves cross-origin requests alone', () => {
    // The waker, when one is configured, is a different origin and not ours
    // to cache.
    expect(strategyFor(req('https://waker.tail1234.ts.net/api/status'))).toBe('network-only');
    expect(strategyFor(req('https://claude.ai/code/abc'))).toBe('network-only');
  });

  it('treats an unparseable URL as network-only', () => {
    expect(strategyFor({ ...req('/'), url: 'not a url' })).toBe('network-only');
  });
});

describe('precacheList', () => {
  it('covers the shell entry points and the wake config', () => {
    const list = precacheList();
    expect(list).toContain('/');
    expect(list).toContain('/manifest.webmanifest');
    expect(list).toContain(CACHEABLE_API_PATH);
  });

  it('does not list hashed assets, which would go stale between builds', () => {
    expect(precacheList().some((p) => p.startsWith('/assets/'))).toBe(false);
  });
});

describe('isCacheable', () => {
  it('stores only a complete same-origin success', () => {
    expect(isCacheable(200, 'basic')).toBe(true);
  });

  it('refuses partial, failed and opaque responses', () => {
    // A 206 would cache a fragment that cannot be served as a whole later.
    expect(isCacheable(206, 'basic')).toBe(false);
    expect(isCacheable(404, 'basic')).toBe(false);
    expect(isCacheable(500, 'basic')).toBe(false);
    expect(isCacheable(302, 'basic')).toBe(false);
    expect(isCacheable(200, 'opaque')).toBe(false);
    expect(isCacheable(200, 'error')).toBe(false);
  });
});

describe('cache versioning', () => {
  it('names the cache after the build', () => {
    expect(cacheName('1.0.0')).toBe('burrow-shell-1.0.0');
    expect(cacheName('dev')).not.toBe(cacheName('1.0.0'));
  });

  it('evicts our previous builds and nothing else', () => {
    const existing = [
      'burrow-shell-0.9.0',
      'burrow-shell-1.0.0',
      'some-other-app-cache',
      'workbox-precache',
    ];
    const stale = stalecaches(existing, 'burrow-shell-1.0.0');
    expect(stale).toEqual(['burrow-shell-0.9.0']);
    expect(stale).not.toContain('some-other-app-cache');
  });
});

describe('serveShellInstead', () => {
  it('serves the cached shell for a lockdown 404 so the app can explain it', () => {
    expect(serveShellInstead(404)).toBe(true);
  });

  it('passes every other answer through', () => {
    for (const status of [200, 204, 304, 401, 403, 429, 500, 502, 504]) {
      expect(serveShellInstead(status)).toBe(false);
    }
  });
});
