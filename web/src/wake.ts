/**
 * The wake and shutdown state machine.
 *
 * This is pure and has no DOM, because getting it wrong has a physical
 * consequence: offering "cut plug power" to a PC that is still running is a
 * hard power-off and whatever Claude was doing is lost. The two rules that
 * matter are:
 *
 *  - "Turn plug off" is offered only after a shutdown *this app asked for*, and
 *    only once /healthz has failed continuously for offConfirmSec. An
 *    unexplained disconnect — a network blip, a crash, Ethernet unplugged —
 *    never produces that prompt.
 *  - "Power-cycle" is offered only after powerCycleAfterSec without the PC
 *    coming online, and always behind an explicit confirmation, because it
 *    cuts power for 15 seconds.
 *
 * See docs/02-ARCHITECTURE.md §3.1 and docs/07-TEST-PLAN.md M2, M2b, M3b.
 */

/** How the PC gets powered on. */
export type WakeMethod = 'smartplug' | 'waker' | 'router' | 'none';

/** Shortcut names as they exist in the iOS Shortcuts app. */
export interface WakeShortcuts {
  on: string;
  off: string;
  powerCycle: string;
}

/** The agent's /api/wake-config payload. */
export interface WakeConfig {
  method: WakeMethod;
  shortcuts: WakeShortcuts;
  wakeTimeoutSec: number;
  powerCycleAfterSec: number;
  offConfirmSec: number;
  wakerUrl: string | null;
  routerHint: string;
  ssh?: { user: string; host: string };
}

/** What the app knows about the PC right now. */
export type Reachability = 'online' | 'offline';

/** The phase the UI renders. */
export type Phase =
  /** Reachable and signed in, or ready to sign in. */
  | 'online'
  /** Not reachable, and we did not ask it to go away. */
  | 'offline'
  /** A wake was requested and we are waiting for it to come up. */
  | 'waking'
  /** Waking has gone on long enough that the plug was probably left on. */
  | 'waking-stalled'
  /** A shutdown was requested and the PC is still answering. */
  | 'shutting-down'
  /** The shutdown is confirmed: /healthz has failed continuously long enough. */
  | 'confirmed-off';

/** Which client this is. The laptop cannot run iOS Shortcuts. */
export type DeviceKind = 'phone' | 'laptop';

/** Persisted across app launches, because the PC reboot outlives the page. */
export interface WakeIntent {
  /** When Wake was tapped. */
  wakeRequestedAt?: number;
  /** When a Burrow shutdown was requested. */
  shutdownRequestedAt?: number;
  /** When /healthz first failed after that shutdown request. */
  unreachableSince?: number;
}

/** Everything the reducer needs. */
export interface WakeState {
  config: WakeConfig;
  deviceKind: DeviceKind;
  reachability: Reachability;
  intent: WakeIntent;
  now: number;
}

/** What the UI should offer. */
export interface WakeView {
  phase: Phase;
  /** Seconds since the wake was requested, for the stepper. */
  elapsedSec: number;
  /** Show the big Wake button. */
  canWake: boolean;
  /** Offer Power-cycle, behind a warning and a confirmation. */
  canPowerCycle: boolean;
  /**
   * Offer "Turn plug off". True only after a Burrow-initiated shutdown that
   * has been confirmed by a continuous run of failed health checks.
   */
  canCutPower: boolean;
  /**
   * On a laptop, Wake cannot run a Shortcut; the user is told to use the
   * iPhone instead. The app still polls for the PC coming online.
   */
  wakeNeedsPhone: boolean;
  /** One line of explanation for the current phase. */
  message: string;
}

/** Wake has no meaning when no wake method is configured. */
function wakeAvailable(config: WakeConfig): boolean {
  return config.method !== 'none';
}

/**
 * Derive what to show. Pure: same inputs, same output, no clocks or storage.
 */
export function wakeView(state: WakeState): WakeView {
  const { config, intent, reachability, now, deviceKind } = state;
  const wakeNeedsPhone = deviceKind === 'laptop' && config.method === 'smartplug';

  const base: WakeView = {
    phase: 'online',
    elapsedSec: 0,
    canWake: false,
    canPowerCycle: false,
    canCutPower: false,
    wakeNeedsPhone,
    message: '',
  };

  // --- the PC is answering ---
  if (reachability === 'online') {
    // A shutdown was asked for but it is still up: it may be running
    // shutdown scripts, or it may have refused because an app objected.
    if (intent.shutdownRequestedAt !== undefined) {
      return {
        ...base,
        phase: 'shutting-down',
        elapsedSec: secondsSince(intent.shutdownRequestedAt, now),
        message: 'Shutting down…',
      };
    }
    return { ...base, phase: 'online', message: 'Online' };
  }

  // --- the PC is not answering, and we asked it to shut down ---
  if (intent.shutdownRequestedAt !== undefined) {
    // The run of failed checks has to be continuous. unreachableSince is
    // cleared by any successful check (see applyHealth), so a blip during the
    // wait restarts the clock rather than counting towards it.
    const since = intent.unreachableSince;
    if (since === undefined) {
      return {
        ...base,
        phase: 'shutting-down',
        elapsedSec: secondsSince(intent.shutdownRequestedAt, now),
        message: 'Shutting down…',
      };
    }
    const downFor = secondsSince(since, now);
    if (downFor < config.offConfirmSec) {
      return {
        ...base,
        phase: 'shutting-down',
        elapsedSec: downFor,
        message: `Shutting down… confirming in ${Math.max(0, config.offConfirmSec - downFor)}s`,
      };
    }
    return {
      ...base,
      phase: 'confirmed-off',
      elapsedSec: downFor,
      // Only here, and only on a phone, does cutting power get offered.
      canCutPower: config.method === 'smartplug' && !wakeNeedsPhone,
      message: 'The PC is off. Cut plug power?',
    };
  }

  // --- the PC is not answering, and a wake is in flight ---
  if (intent.wakeRequestedAt !== undefined) {
    const elapsed = secondsSince(intent.wakeRequestedAt, now);
    if (elapsed >= config.powerCycleAfterSec) {
      return {
        ...base,
        phase: 'waking-stalled',
        elapsedSec: elapsed,
        canWake: wakeAvailable(config),
        canPowerCycle: config.method === 'smartplug' && !wakeNeedsPhone,
        message: 'Still off? The plug may have been left on.',
      };
    }
    return {
      ...base,
      phase: 'waking',
      elapsedSec: elapsed,
      message: elapsed < 10 ? 'Powering on…' : 'Booting…',
    };
  }

  // --- not answering, nothing requested: a plain offline state ---
  return {
    ...base,
    phase: 'offline',
    canWake: wakeAvailable(config),
    message: 'The PC is off.',
  };
}

function secondsSince(from: number, now: number): number {
  return Math.max(0, Math.floor((now - from) / 1000));
}

/**
 * Fold a health-check result into the intent.
 *
 * The important half is the failure path: a failed check only starts the
 * unreachable clock when a shutdown was actually requested. Without that, an
 * ordinary disconnect would walk the state machine towards offering to cut
 * power (docs/07-TEST-PLAN.md M2b).
 */
export function applyHealth(intent: WakeIntent, healthy: boolean, now: number): WakeIntent {
  if (healthy) {
    const next: WakeIntent = {};
    // A successful check ends a wake: the PC is up.
    // It also clears any run of failures, so the confirm window has to start
    // over if the PC goes away again.
    if (intent.shutdownRequestedAt !== undefined) {
      next.shutdownRequestedAt = intent.shutdownRequestedAt;
    }
    return next;
  }

  if (intent.shutdownRequestedAt === undefined) {
    // Unexplained disconnect: remember nothing. This is what stops the app
    // ever offering to cut power after a network blip.
    return { ...intent };
  }
  if (intent.unreachableSince !== undefined) {
    return intent; // already counting
  }
  return { ...intent, unreachableSince: now };
}

/** Record that Wake was tapped. */
export function requestWake(intent: WakeIntent, now: number): WakeIntent {
  return { ...intent, wakeRequestedAt: now };
}

/** Record that a Burrow shutdown was requested. */
export function requestShutdown(_intent: WakeIntent, now: number): WakeIntent {
  // A shutdown replaces any wake in flight rather than merging with it: the
  // two cannot be true at once, and a leftover wakeRequestedAt would make the
  // UI offer Power-cycle during a shutdown.
  return { shutdownRequestedAt: now };
}

/**
 * Clear the intent. Called when the user dismisses the prompt, when the plug
 * has been turned off, or when a wake has run past its timeout.
 */
export function clearIntent(): WakeIntent {
  return {};
}

/** True once a wake has run past wakeTimeoutSec and should be given up on. */
export function wakeTimedOut(state: WakeState): boolean {
  const at = state.intent.wakeRequestedAt;
  if (at === undefined) return false;
  return secondsSince(at, state.now) >= state.config.wakeTimeoutSec;
}

/**
 * Build the URL that runs an iOS Shortcut.
 *
 * The name is percent-encoded because the shortcuts the runbook creates have
 * spaces in them ("PC On"), and an unencoded space silently fails to match.
 */
export function shortcutURL(name: string): string {
  return `shortcuts://run-shortcut?name=${encodeURIComponent(name)}`;
}

/** Storage key for the persisted intent. */
export const INTENT_KEY = 'burrow.wakeIntent';

/**
 * Read the persisted intent.
 *
 * Storage can throw (private browsing, blocked site data) and can hold
 * anything, so a bad value is treated as no intent rather than propagated: the
 * worst case of a wrong intent here is an unwanted "cut power" prompt.
 */
export function loadIntent(storage: Pick<Storage, 'getItem'>): WakeIntent {
  try {
    const raw = storage.getItem(INTENT_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null) return {};
    const out: WakeIntent = {};
    const obj = parsed as Record<string, unknown>;
    if (typeof obj.wakeRequestedAt === 'number') out.wakeRequestedAt = obj.wakeRequestedAt;
    if (typeof obj.shutdownRequestedAt === 'number') {
      out.shutdownRequestedAt = obj.shutdownRequestedAt;
    }
    if (typeof obj.unreachableSince === 'number') out.unreachableSince = obj.unreachableSince;
    return out;
  } catch {
    return {};
  }
}

/** Persist the intent, ignoring storage failures. */
export function saveIntent(storage: Pick<Storage, 'setItem'>, intent: WakeIntent): void {
  try {
    storage.setItem(INTENT_KEY, JSON.stringify(intent));
  } catch {
    // Nothing to do: the app still works, it just forgets across launches.
  }
}
