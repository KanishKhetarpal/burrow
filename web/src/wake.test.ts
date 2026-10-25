import { describe, expect, it } from 'vitest';
import {
  applyHealth,
  clearIntent,
  INTENT_KEY,
  loadIntent,
  requestShutdown,
  requestWake,
  saveIntent,
  shortcutURL,
  wakeTimedOut,
  wakeView,
  type DeviceKind,
  type Reachability,
  type WakeConfig,
  type WakeIntent,
  type WakeState,
} from './wake';

const config: WakeConfig = {
  method: 'smartplug',
  shortcuts: { on: 'PC On', off: 'PC Off', powerCycle: 'PC Power Cycle' },
  wakeTimeoutSec: 180,
  powerCycleAfterSec: 90,
  offConfirmSec: 30,
  wakerUrl: null,
  routerHint: '',
};

const T0 = 1_700_000_000_000;

function state(
  overrides: Partial<WakeState> & { reachability: Reachability; intent: WakeIntent },
): WakeState {
  return {
    config,
    deviceKind: 'phone' as DeviceKind,
    now: T0,
    ...overrides,
  };
}

const secs = (n: number) => n * 1000;

describe('wakeView — online', () => {
  it('reports online with nothing offered', () => {
    const v = wakeView(state({ reachability: 'online', intent: {} }));
    expect(v.phase).toBe('online');
    expect(v.canWake).toBe(false);
    expect(v.canCutPower).toBe(false);
    expect(v.canPowerCycle).toBe(false);
  });

  it('shows shutting-down while the PC is still answering', () => {
    const v = wakeView(
      state({
        reachability: 'online',
        intent: { shutdownRequestedAt: T0 - secs(5) },
        now: T0,
      }),
    );
    expect(v.phase).toBe('shutting-down');
    // Still up, so cutting power would be a hard power-off.
    expect(v.canCutPower).toBe(false);
  });
});

describe('wakeView — offline', () => {
  it('offers Wake when nothing is in flight', () => {
    const v = wakeView(state({ reachability: 'offline', intent: {} }));
    expect(v.phase).toBe('offline');
    expect(v.canWake).toBe(true);
    expect(v.canCutPower).toBe(false);
  });

  it('offers no Wake when no wake method is configured', () => {
    const v = wakeView(
      state({ reachability: 'offline', intent: {}, config: { ...config, method: 'none' } }),
    );
    expect(v.canWake).toBe(false);
  });
});

describe('wakeView — waking', () => {
  it('shows progress before the power-cycle threshold', () => {
    const v = wakeView(
      state({ reachability: 'offline', intent: { wakeRequestedAt: T0 - secs(20) }, now: T0 }),
    );
    expect(v.phase).toBe('waking');
    expect(v.elapsedSec).toBe(20);
    expect(v.canPowerCycle).toBe(false);
  });

  it('offers Power-cycle only after powerCycleAfterSec', () => {
    const justBefore = wakeView(
      state({ reachability: 'offline', intent: { wakeRequestedAt: T0 - secs(89) }, now: T0 }),
    );
    expect(justBefore.canPowerCycle).toBe(false);

    const atThreshold = wakeView(
      state({ reachability: 'offline', intent: { wakeRequestedAt: T0 - secs(90) }, now: T0 }),
    );
    expect(atThreshold.phase).toBe('waking-stalled');
    expect(atThreshold.canPowerCycle).toBe(true);
    // Wake is still offered, since the plug may simply not have been on.
    expect(atThreshold.canWake).toBe(true);
  });

  it('does not offer Power-cycle on a laptop', () => {
    const v = wakeView(
      state({
        reachability: 'offline',
        intent: { wakeRequestedAt: T0 - secs(120) },
        now: T0,
        deviceKind: 'laptop',
      }),
    );
    expect(v.phase).toBe('waking-stalled');
    expect(v.canPowerCycle).toBe(false);
    expect(v.wakeNeedsPhone).toBe(true);
  });
});

describe('wakeView — the cut-power rule', () => {
  it('offers to cut power only after a confirmed Burrow shutdown', () => {
    const intent = applyHealth({ shutdownRequestedAt: T0 - secs(60) }, false, T0 - secs(40));
    const v = wakeView(state({ reachability: 'offline', intent, now: T0 }));

    expect(v.phase).toBe('confirmed-off');
    expect(v.canCutPower).toBe(true);
  });

  it('waits the full offConfirmSec before offering it', () => {
    const intent: WakeIntent = {
      shutdownRequestedAt: T0 - secs(60),
      unreachableSince: T0 - secs(29),
    };
    const v = wakeView(state({ reachability: 'offline', intent, now: T0 }));
    expect(v.phase).toBe('shutting-down');
    expect(v.canCutPower).toBe(false);

    const later = wakeView(state({ reachability: 'offline', intent, now: T0 + secs(2) }));
    expect(later.phase).toBe('confirmed-off');
    expect(later.canCutPower).toBe(true);
  });

  // This is M2b: the single most important negative case. An unexplained
  // disconnect must never walk towards offering to cut power.
  it('never offers to cut power after an unexplained disconnect', () => {
    let intent: WakeIntent = {};
    // Ten minutes of failed checks with no shutdown request.
    for (let i = 0; i < 60; i++) {
      intent = applyHealth(intent, false, T0 + secs(i * 10));
    }
    const v = wakeView(state({ reachability: 'offline', intent, now: T0 + secs(600) }));

    expect(v.canCutPower).toBe(false);
    expect(v.phase).toBe('offline');
    expect(intent.unreachableSince).toBeUndefined();
  });

  it('never offers to cut power after a wake, only after a shutdown', () => {
    let intent = requestWake({}, T0);
    for (let i = 0; i < 30; i++) {
      intent = applyHealth(intent, false, T0 + secs(i * 5));
    }
    const v = wakeView(state({ reachability: 'offline', intent, now: T0 + secs(150) }));
    expect(v.canCutPower).toBe(false);
  });

  // A blip during the confirm window restarts the clock, so "continuously
  // failing for 30s" means what it says.
  it('restarts the confirm window if the PC answers again', () => {
    let intent = requestShutdown({}, T0);
    intent = applyHealth(intent, false, T0 + secs(5));
    expect(intent.unreachableSince).toBe(T0 + secs(5));

    // One successful check 20 seconds in.
    intent = applyHealth(intent, true, T0 + secs(25));
    expect(intent.unreachableSince).toBeUndefined();
    expect(intent.shutdownRequestedAt).toBe(T0);

    // Fails again; the window starts from here, not from the first failure.
    intent = applyHealth(intent, false, T0 + secs(30));
    expect(intent.unreachableSince).toBe(T0 + secs(30));

    // 25s after the restart is not yet enough, even though it is 55s since
    // the first failure.
    const tooEarly = wakeView(state({ reachability: 'offline', intent, now: T0 + secs(55) }));
    expect(tooEarly.canCutPower).toBe(false);

    const enough = wakeView(state({ reachability: 'offline', intent, now: T0 + secs(61) }));
    expect(enough.canCutPower).toBe(true);
  });

  it('does not offer to cut power on a laptop', () => {
    const intent: WakeIntent = {
      shutdownRequestedAt: T0 - secs(120),
      unreachableSince: T0 - secs(60),
    };
    const v = wakeView(
      state({ reachability: 'offline', intent, now: T0, deviceKind: 'laptop' }),
    );
    expect(v.phase).toBe('confirmed-off');
    expect(v.canCutPower).toBe(false);
  });

  it('does not offer to cut power when the method is not a smart plug', () => {
    const intent: WakeIntent = {
      shutdownRequestedAt: T0 - secs(120),
      unreachableSince: T0 - secs(60),
    };
    const v = wakeView(
      state({
        reachability: 'offline',
        intent,
        now: T0,
        config: { ...config, method: 'waker' },
      }),
    );
    expect(v.canCutPower).toBe(false);
  });
});

describe('applyHealth', () => {
  it('clears a wake once the PC answers', () => {
    const intent = applyHealth(requestWake({}, T0), true, T0 + secs(40));
    expect(intent.wakeRequestedAt).toBeUndefined();
  });

  it('keeps the shutdown request when the PC answers', () => {
    // The PC may answer once more while it is shutting down; that does not
    // mean the shutdown was abandoned.
    const intent = applyHealth({ shutdownRequestedAt: T0 }, true, T0 + secs(2));
    expect(intent.shutdownRequestedAt).toBe(T0);
  });

  it('does not move unreachableSince once it is set', () => {
    let intent = requestShutdown({}, T0);
    intent = applyHealth(intent, false, T0 + secs(5));
    intent = applyHealth(intent, false, T0 + secs(10));
    intent = applyHealth(intent, false, T0 + secs(15));
    expect(intent.unreachableSince).toBe(T0 + secs(5));
  });
});

describe('intent transitions', () => {
  it('requestShutdown replaces a wake in flight', () => {
    const intent = requestShutdown(requestWake({}, T0), T0 + secs(5));
    expect(intent.wakeRequestedAt).toBeUndefined();
    expect(intent.shutdownRequestedAt).toBe(T0 + secs(5));
  });

  it('clearIntent empties everything', () => {
    expect(clearIntent()).toEqual({});
  });

  it('wakeTimedOut fires at wakeTimeoutSec', () => {
    const intent = requestWake({}, T0);
    expect(wakeTimedOut(state({ reachability: 'offline', intent, now: T0 + secs(179) }))).toBe(false);
    expect(wakeTimedOut(state({ reachability: 'offline', intent, now: T0 + secs(180) }))).toBe(true);
    expect(wakeTimedOut(state({ reachability: 'offline', intent: {}, now: T0 }))).toBe(false);
  });
});

describe('shortcutURL', () => {
  it('encodes spaces, which the runbook shortcut names contain', () => {
    expect(shortcutURL('PC On')).toBe('shortcuts://run-shortcut?name=PC%20On');
    expect(shortcutURL('PC Power Cycle')).toBe('shortcuts://run-shortcut?name=PC%20Power%20Cycle');
  });

  it('encodes characters that would break the URL', () => {
    expect(shortcutURL('A&B?C')).toBe('shortcuts://run-shortcut?name=A%26B%3FC');
  });
});

describe('intent storage', () => {
  function memStorage(initial?: string) {
    const map = new Map<string, string>();
    if (initial !== undefined) map.set(INTENT_KEY, initial);
    return {
      getItem: (k: string) => map.get(k) ?? null,
      setItem: (k: string, v: string) => void map.set(k, v),
      read: () => map.get(INTENT_KEY),
    };
  }

  it('round-trips an intent', () => {
    const s = memStorage();
    const intent: WakeIntent = { shutdownRequestedAt: T0, unreachableSince: T0 + 1000 };
    saveIntent(s, intent);
    expect(loadIntent(s)).toEqual(intent);
  });

  it('treats unreadable or malformed storage as no intent', () => {
    expect(loadIntent(memStorage('not json'))).toEqual({});
    expect(loadIntent(memStorage('null'))).toEqual({});
    expect(loadIntent(memStorage('"a string"'))).toEqual({});
    expect(loadIntent(memStorage('[1,2,3]'))).toEqual({});
    expect(loadIntent(memStorage())).toEqual({});
  });

  it('ignores fields of the wrong type rather than trusting them', () => {
    const s = memStorage(JSON.stringify({ shutdownRequestedAt: 'yesterday', unreachableSince: {} }));
    expect(loadIntent(s)).toEqual({});
  });

  it('survives storage that throws', () => {
    const throwing = {
      getItem: () => {
        throw new Error('blocked');
      },
      setItem: () => {
        throw new Error('blocked');
      },
    };
    expect(loadIntent(throwing)).toEqual({});
    expect(() => saveIntent(throwing, { wakeRequestedAt: T0 })).not.toThrow();
  });
});
