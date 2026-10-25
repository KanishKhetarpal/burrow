/**
 * Typed client for the agent's API (docs/04-API.md).
 *
 * Every state-changing call carries the X-Burrow header the server requires,
 * and a `stepup_required` response is turned into a typed error the caller can
 * recover from by running the step-up flow and retrying — which is what makes
 * Face ID feel like part of the action rather than a separate chore.
 */

import type { WakeConfig } from './wake';

/** Error codes the UI branches on. */
export type ErrorCode =
  | 'auth_required'
  | 'stepup_required'
  | 'csrf'
  | 'rate_limited'
  | 'locked_out'
  | 'bad_request'
  | 'not_found'
  | 'conflict'
  | 'max_sessions'
  | 'sessions_running'
  | 'too_large'
  | 'forbidden'
  | 'internal';

/** An error carrying the server's machine-readable code. */
export class ApiError extends Error {
  readonly code: ErrorCode | string;
  readonly status: number;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }

  /** The session has gone; the app should show the unlock screen. */
  get needsAuth(): boolean {
    return this.code === 'auth_required';
  }

  /** A fresh passkey assertion is needed before this action is allowed. */
  get needsStepup(): boolean {
    return this.code === 'stepup_required';
  }
}

/** Raised when the agent could not be reached at all. */
export class OfflineError extends Error {
  constructor(message = 'The PC did not answer') {
    super(message);
    this.name = 'OfflineError';
  }
}

/**
 * How long to wait before treating the agent as unreachable.
 *
 * Four seconds matches the service worker's navigation timeout: the app has to
 * decide "offline" quickly, because the whole point of the offline screen is to
 * show a Wake button rather than a spinner (docs/02-ARCHITECTURE.md §2.3).
 */
export const REQUEST_TIMEOUT_MS = 4000;

/** A longer budget for calls that legitimately take a while. */
const SLOW_TIMEOUT_MS = 30_000;

interface RequestOptions {
  method?: string;
  body?: unknown;
  /** Extra headers, e.g. the ceremony id for a WebAuthn finish call. */
  headers?: Record<string, string>;
  timeoutMs?: number;
  signal?: AbortSignal;
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const method = opts.method ?? 'GET';
  const headers: Record<string, string> = { ...opts.headers };

  if (method !== 'GET' && method !== 'HEAD') {
    // The server requires this on every state-changing request; a cross-site
    // form post cannot set it.
    headers['X-Burrow'] = '1';
  }
  if (opts.body !== undefined && !(opts.body instanceof FormData)) {
    headers['Content-Type'] = 'application/json';
  }

  const timeout = AbortSignal.timeout(opts.timeoutMs ?? REQUEST_TIMEOUT_MS);
  const signal = opts.signal ? AbortSignal.any([timeout, opts.signal]) : timeout;

  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers,
      signal,
      // Same-origin only. The cookie is __Host- prefixed and SameSite=Strict,
      // so it would not travel cross-origin anyway.
      credentials: 'same-origin',
      cache: 'no-store',
      body:
        opts.body === undefined
          ? null
          : opts.body instanceof FormData
            ? opts.body
            : JSON.stringify(opts.body),
    });
  } catch (e) {
    // A timeout and a dead network look the same here, and both mean the same
    // thing to the user.
    throw new OfflineError(e instanceof Error ? e.message : undefined);
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let parsed: unknown = undefined;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = undefined;
    }
  }

  if (!res.ok) {
    const envelope = parsed as { error?: { code?: string; message?: string } } | undefined;
    throw new ApiError(
      res.status,
      envelope?.error?.code ?? `http_${res.status}`,
      envelope?.error?.message ?? res.statusText ?? 'Request failed',
    );
  }
  return parsed as T;
}

/* ---------- health and bootstrap ---------- */

export interface Health {
  ok: boolean;
  version: string;
}

/** Probe the agent. Resolves false rather than throwing when it is down. */
export async function health(): Promise<Health | null> {
  try {
    return await request<Health>('/healthz');
  } catch (e) {
    // Any HTTP answer means the PC is on and Burrow is running — a 429 or a
    // 500 included. Reading those as "off" offered to cut the plug's power
    // on a PC that was merely busy. Only no answer at all is offline.
    if (e instanceof ApiError && e.status !== 404) return { ok: false, version: '' };
    return null;
  }
}

export const getWakeConfig = () => request<WakeConfig>('/api/wake-config');

/* ---------- auth ---------- */

export interface AuthState {
  paired: boolean;
  pairingOpen: boolean;
  authenticated: boolean;
  lockdown: boolean;
  maxDevices: number;
}

export const getAuthState = () => request<AuthState>('/api/auth/state');

export interface Me {
  login: string;
  device: { id: string; name: string; kind: 'phone' | 'laptop' };
  stepupValidUntil: string | null;
  /** The PC allows bypassPermissions sessions (claude.allow_bypass_permissions). */
  bypassPermissionsAllowed?: boolean;
}

export const getMe = () => request<Me>('/api/auth/me');

/** A WebAuthn challenge plus the ceremony id that goes back with the answer. */
export interface Ceremony<T> {
  ceremonyId: string;
  publicKey: T;
}

export const loginBegin = () =>
  request<Ceremony<PublicKeyCredentialRequestOptionsJSON>>('/api/auth/login/begin', {
    method: 'POST',
  });

export const loginFinish = (ceremonyId: string, credential: unknown) =>
  request<{ ok: true; device: Me['device'] }>('/api/auth/login/finish', {
    method: 'POST',
    body: credential,
    headers: { 'X-Burrow-Ceremony': ceremonyId },
  });

export const stepupBegin = () =>
  request<Ceremony<PublicKeyCredentialRequestOptionsJSON>>('/api/auth/stepup/begin', {
    method: 'POST',
  });

export const stepupFinish = (ceremonyId: string, credential: unknown) =>
  request<{ ok: true }>('/api/auth/stepup/finish', {
    method: 'POST',
    body: credential,
    headers: { 'X-Burrow-Ceremony': ceremonyId },
  });

export const pairBegin = (code: string, deviceName: string, kind: 'phone' | 'laptop') =>
  request<Ceremony<PublicKeyCredentialCreationOptionsJSON>>('/api/auth/pair/begin', {
    method: 'POST',
    body: { code, deviceName, kind },
  });

export const pairFinish = (ceremonyId: string, credential: unknown) =>
  request<{ ok: true; signedIn: boolean }>('/api/auth/pair/finish', {
    method: 'POST',
    body: credential,
    headers: { 'X-Burrow-Ceremony': ceremonyId },
  });

export const logout = () => request<{ ok: true }>('/api/auth/logout', { method: 'POST' });

export const addSSHKey = (publicKey: string) =>
  request<{ ok: true; fingerprint: string }>('/api/ssh-keys', {
    method: 'POST',
    body: { publicKey },
  });

/* ---------- system and power ---------- */

export interface Disk {
  mount: string;
  usedGB: number;
  totalGB: number;
}

export interface KeepAwake {
  active: boolean;
  reason?: 'sessions' | 'manual';
  until: string | null;
}

export interface SystemInfo {
  hostname: string;
  osVersion: string;
  uptimeSec: number;
  cpuPct: number;
  ramUsedMB: number;
  ramTotalMB: number;
  disks: Disk[];
  keepAwake: KeepAwake;
  sessions: { running: number };
  tailscale: { online: boolean };
  version: string;
  pendingPower: { action: string; scheduledAt: string; plugOffSafe: boolean } | null;
}

export const getSystem = () => request<SystemInfo>('/api/system');

export type PowerAction = 'sleep' | 'hibernate' | 'shutdown' | 'restart' | 'lock';

export interface PowerResult {
  ok: true;
  scheduledAt: string;
  plugOffSafe: boolean;
  wakeable: boolean;
}

export const power = (action: PowerAction, delaySec = 0, force = false) =>
  request<PowerResult>('/api/power', {
    method: 'POST',
    body: { action, delaySec, force },
    timeoutMs: SLOW_TIMEOUT_MS,
  });

export const setKeepAwake = (enabled: boolean, minutes = 0) =>
  request<{ ok: true; keepAwake: KeepAwake }>('/api/power/keep-awake', {
    method: 'POST',
    body: { enabled, minutes },
  });

export const lockdown = () =>
  request<{ ok: true; couldNotStop: string[] | null }>('/api/lockdown', {
    method: 'POST',
    timeoutMs: SLOW_TIMEOUT_MS,
  });

/* ---------- projects and sessions ---------- */

export interface Project {
  id: string;
  name: string;
  path?: string;
  isGit: boolean;
}

export const getProjects = () => request<Project[]>('/api/projects');

/** A one-tap button defined in burrow.yaml (e.g. one per Claude account). */
export interface Launcher {
  name: string;
  projectId?: string;
  permissionMode?: 'default' | 'acceptEdits' | 'plan' | 'bypassPermissions';
  /** The button has its own folder: one tap starts the session. */
  oneTap: boolean;
}

export const getLaunchers = () => request<Launcher[]>('/api/launchers');

/** An "Open on PC" button from burrow.yaml. */
export interface PcApp {
  name: string;
}

export const getApps = () => request<PcApp[]>('/api/apps');

export const openApp = (name: string) =>
  request<void>(`/api/apps/${encodeURIComponent(name)}/open`, { method: 'POST' });

/** A folder inside one of projects_parents that could be added. Step-up. */
export interface ProjectCandidate {
  name: string;
  path: string;
  parent: string;
  isGit: boolean;
  added: boolean;
}

export const getProjectCandidates = () =>
  request<ProjectCandidate[]>('/api/projects/candidates');

export const addProject = (path: string, name?: string) =>
  request<Project>('/api/projects', { method: 'POST', body: { path, name } });

export const removeProject = (id: string) =>
  request<void>(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE' });

export type SessionStatus =
  | 'starting'
  | 'ready'
  | 'needs_input'
  | 'running'
  | 'exited'
  | 'failed'
  | 'stopped';

export interface SessionPrompt {
  kind: 'trust' | 'enable_rc' | 'permission' | 'other';
  text: string;
  options?: string[];
}

export interface Session {
  id: string;
  name: string;
  projectId: string;
  cwd: string;
  mode: 'chat' | 'server';
  permissionMode: 'default' | 'acceptEdits' | 'plan' | 'bypassPermissions';
  spawn?: 'same-dir' | 'worktree';
  status: SessionStatus;
  remoteUrl?: string;
  prompt?: SessionPrompt;
  pid?: number;
  startedAt: string;
  endedAt: string | null;
  exitCode: number | null;
  restarts: number;
}

export const getSessions = () => request<Session[]>('/api/sessions');
export const getSession = (id: string) => request<Session>(`/api/sessions/${id}`);

export interface CreateSessionRequest {
  projectId: string;
  /** A launcher from burrow.yaml; picks the Claude login for the session. */
  launcher?: string;
  name?: string;
  mode?: 'chat' | 'server';
  /** bypassPermissions is intentionally absent from this type. */
  permissionMode?: 'default' | 'acceptEdits' | 'plan' | 'bypassPermissions';
  spawn?: 'same-dir' | 'worktree';
}

export const createSession = (body: CreateSessionRequest) =>
  request<Session>('/api/sessions', { method: 'POST', body, timeoutMs: SLOW_TIMEOUT_MS });

export const sendSessionText = (id: string, text: string) =>
  request<{ ok: true }>(`/api/sessions/${id}/input`, { method: 'POST', body: { text } });

export const sendSessionKey = (id: string, key: string) =>
  request<{ ok: true }>(`/api/sessions/${id}/input`, { method: 'POST', body: { key } });

export const stopSession = (id: string) =>
  request<{ ok: true }>(`/api/sessions/${id}/stop`, {
    method: 'POST',
    timeoutMs: SLOW_TIMEOUT_MS,
  });

export const resumeSession = (id: string) =>
  request<Session>(`/api/sessions/${id}/resume`, { method: 'POST', timeoutMs: SLOW_TIMEOUT_MS });

export const deleteSession = (id: string) =>
  request<{ ok: true }>(`/api/sessions/${id}`, { method: 'DELETE' });

export const killAllSessions = () =>
  request<{ ok: true; killed: number }>('/api/sessions/kill-all', { method: 'POST' });

export const getScrollback = (id: string) =>
  request<{ bytes: string; len: number }>(`/api/sessions/${id}/scrollback`, {
    timeoutMs: SLOW_TIMEOUT_MS,
  });

/* ---------- files ---------- */

export interface FileRoot {
  name: string;
  writable: boolean;
  path?: string;
}

export const getFileRoots = () => request<FileRoot[]>('/api/files/roots');

export interface FileEntry {
  name: string;
  type: 'file' | 'dir';
  size: number;
  mtime: string;
  previewable: boolean;
}

export const listFiles = (root: string, path: string, sort = 'name') =>
  request<{ entries: FileEntry[] }>(
    `/api/files/list?root=${encodeURIComponent(root)}&path=${encodeURIComponent(path)}&sort=${sort}`,
  );

export const fileURL = (kind: 'download' | 'preview', root: string, path: string) =>
  `/api/files/${kind}?root=${encodeURIComponent(root)}&path=${encodeURIComponent(path)}`;

export const mkdir = (root: string, path: string) =>
  request<{ ok: true }>('/api/files/mkdir', { method: 'POST', body: { root, path } });

export const renameFile = (root: string, from: string, to: string) =>
  request<{ ok: true }>('/api/files/rename', { method: 'POST', body: { root, from, to } });

export const deleteFile = (root: string, path: string) =>
  request<{ ok: true; recycled: boolean }>('/api/files/delete', {
    method: 'POST',
    body: { root, path },
  });

export const searchFiles = (root: string, q: string) =>
  request<{ results: string[] }>(
    `/api/files/search?root=${encodeURIComponent(root)}&q=${encodeURIComponent(q)}`,
    { timeoutMs: SLOW_TIMEOUT_MS },
  );

/**
 * Upload files with progress.
 *
 * XMLHttpRequest rather than fetch, because fetch still has no upload progress
 * event and a 2 GB upload from a phone with no progress bar is unusable.
 */
export function uploadFiles(
  root: string,
  path: string,
  files: File[],
  onProgress?: (fraction: number) => void,
  overwrite = false,
): Promise<{ ok: true; uploaded: { name: string; bytes: number }[] }> {
  const form = new FormData();
  for (const f of files) form.append('files', f, f.name);

  const url =
    `/api/files/upload?root=${encodeURIComponent(root)}` +
    `&path=${encodeURIComponent(path)}&overwrite=${overwrite ? 'true' : 'false'}`;

  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    xhr.setRequestHeader('X-Burrow', '1');
    xhr.withCredentials = false; // same-origin; the cookie travels anyway

    if (onProgress) {
      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable) onProgress(e.loaded / e.total);
      });
    }
    xhr.addEventListener('load', () => {
      let parsed: unknown;
      try {
        parsed = JSON.parse(xhr.responseText);
      } catch {
        parsed = undefined;
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(parsed as { ok: true; uploaded: { name: string; bytes: number }[] });
        return;
      }
      const envelope = parsed as { error?: { code?: string; message?: string } } | undefined;
      reject(
        new ApiError(
          xhr.status,
          envelope?.error?.code ?? `http_${xhr.status}`,
          envelope?.error?.message ?? 'Upload failed',
        ),
      );
    });
    xhr.addEventListener('error', () => reject(new OfflineError('The upload could not be sent')));
    xhr.addEventListener('abort', () => reject(new OfflineError('The upload was cancelled')));
    xhr.send(form);
  });
}

/* ---------- security ---------- */

export interface AuditEntry {
  seq: number;
  ts: string;
  actor: string;
  action: string;
  target?: string;
  result: 'ok' | 'denied' | 'error';
  meta?: Record<string, unknown>;
}

export const getAudit = (limit = 200, before?: number) =>
  request<{ entries: AuditEntry[] }>(
    `/api/audit?limit=${limit}${before !== undefined ? `&before=${before}` : ''}`,
  );

export interface Device {
  id: string;
  name: string;
  kind: 'phone' | 'laptop';
  nodeTail: string;
  current: boolean;
}

export const getDevices = () => request<Device[]>('/api/devices');

export const revokeDevice = (id: string) =>
  request<{ ok: true }>(`/api/devices/${encodeURIComponent(id)}`, { method: 'DELETE' });

/* ---------- WebAuthn JSON shapes ---------- */
// Declared locally rather than imported from @simplewebauthn/browser, so the
// API surface here does not change shape when that library's types move.

export interface PublicKeyCredentialRequestOptionsJSON {
  challenge: string;
  timeout?: number;
  rpId?: string;
  allowCredentials?: { id: string; type: 'public-key'; transports?: string[] }[];
  userVerification?: 'required' | 'preferred' | 'discouraged';
  extensions?: Record<string, unknown>;
}

export interface PublicKeyCredentialCreationOptionsJSON {
  rp: { id?: string; name: string };
  user: { id: string; name: string; displayName: string };
  challenge: string;
  pubKeyCredParams: { alg: number; type: 'public-key' }[];
  timeout?: number;
  excludeCredentials?: { id: string; type: 'public-key'; transports?: string[] }[];
  authenticatorSelection?: {
    authenticatorAttachment?: 'platform' | 'cross-platform';
    residentKey?: 'discouraged' | 'preferred' | 'required';
    requireResidentKey?: boolean;
    userVerification?: 'required' | 'preferred' | 'discouraged';
  };
  attestation?: 'none' | 'indirect' | 'direct' | 'enterprise';
  extensions?: Record<string, unknown>;
}
