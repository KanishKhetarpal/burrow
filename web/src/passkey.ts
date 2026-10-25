/**
 * Passkey flows and the step-up retry wrapper.
 *
 * Step-up is the part worth getting right in the UI: the server refuses a
 * dangerous action with `stepup_required`, the app runs a Face ID prompt and
 * retries the same call. From the user's side that is one Face ID prompt in the
 * middle of an action, not an error they have to understand and recover from
 * (docs/05-UI.md, States and errors).
 */

import { startAuthentication, startRegistration } from '@simplewebauthn/browser';
import * as api from './api';
import { ApiError } from './api';

/** True when this browser can do platform passkeys at all. */
export function passkeysAvailable(): boolean {
  return typeof window !== 'undefined' && 'PublicKeyCredential' in window;
}

/** A cancelled passkey prompt, which is a normal thing for a user to do. */
export class PasskeyCancelled extends Error {
  constructor() {
    super('Cancelled');
    this.name = 'PasskeyCancelled';
  }
}

function asPasskeyError(e: unknown): Error {
  if (e instanceof ApiError) return e;
  if (e instanceof Error) {
    // The browser reports a cancelled or timed-out prompt as NotAllowedError;
    // it is not a failure worth showing as one.
    if (e.name === 'NotAllowedError' || e.name === 'AbortError') return new PasskeyCancelled();
    return e;
  }
  return new Error('The passkey prompt failed');
}

/** Sign in with Face ID / Windows Hello / Touch ID. */
export async function login(): Promise<api.Me['device']> {
  try {
    const { ceremonyId, publicKey } = await api.loginBegin();
    const credential = await startAuthentication({ optionsJSON: publicKey as never });
    const res = await api.loginFinish(ceremonyId, credential);
    return res.device;
  } catch (e) {
    throw asPasskeyError(e);
  }
}

/** Register this device during pairing. */
export async function pair(
  code: string,
  deviceName: string,
  kind: 'phone' | 'laptop',
): Promise<{ signedIn: boolean }> {
  try {
    const { ceremonyId, publicKey } = await api.pairBegin(code, deviceName, kind);
    const credential = await startRegistration({ optionsJSON: publicKey as never });
    const res = await api.pairFinish(ceremonyId, credential);
    return { signedIn: res.signedIn };
  } catch (e) {
    throw asPasskeyError(e);
  }
}

/** Prove presence again, for a dangerous action. */
export async function stepUp(): Promise<void> {
  try {
    const { ceremonyId, publicKey } = await api.stepupBegin();
    const credential = await startAuthentication({ optionsJSON: publicKey as never });
    await api.stepupFinish(ceremonyId, credential);
  } catch (e) {
    throw asPasskeyError(e);
  }
}

/**
 * Run an action, stepping up and retrying once if the server asks for it.
 *
 * Retried exactly once: if the second attempt still comes back needing
 * step-up, something is wrong beyond a stale assertion and looping would just
 * prompt for Face ID forever.
 */
export async function withStepUp<T>(action: () => Promise<T>): Promise<T> {
  try {
    return await action();
  } catch (e) {
    if (!(e instanceof ApiError) || !e.needsStepup) throw e;
    await stepUp();
    return action();
  }
}

/**
 * Guess what kind of device this is, for the pairing screen's default.
 *
 * Only a default for a field the user can change — nothing is decided on it,
 * and the server stores whatever is submitted.
 */
export function guessDeviceKind(): 'phone' | 'laptop' {
  if (typeof navigator === 'undefined') return 'laptop';
  const ua = navigator.userAgent;
  const touch = navigator.maxTouchPoints > 1;
  if (/iPhone|iPod|Android.*Mobile/.test(ua)) return 'phone';
  // An iPad reports a desktop user agent but has touch points; either answer
  // is defensible, and "laptop" matches how it would be used here.
  if (/iPad/.test(ua) || (/Macintosh/.test(ua) && touch)) return 'laptop';
  return 'laptop';
}

/** A sensible default name for the device being paired. */
export function guessDeviceName(kind: 'phone' | 'laptop'): string {
  if (kind === 'phone') return 'iPhone';
  if (typeof navigator !== 'undefined' && /Macintosh/.test(navigator.userAgent)) return 'MacBook';
  return 'Laptop';
}
