/**
 * The app shell: connectivity, auth gate, tabs.
 *
 * One poll loop owns reachability, and it feeds the wake state machine in
 * wake.ts. Keeping that in one place is what makes the "never offer to cut
 * power after an unexplained disconnect" rule hold — there is exactly one
 * writer of the intent.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks';

import * as api from './api';
import { ApiError } from './api';
import * as passkey from './passkey';
import {
  applyHealth,
  clearIntent,
  loadIntent,
  requestShutdown,
  requestWake,
  saveIntent,
  shortcutURL,
  wakeTimedOut,
  wakeView,
  type DeviceKind,
  type WakeConfig,
  type WakeIntent,
  type WakeView,
} from './wake';
import { Toasts, type Toast } from './components/ui';
import { BurrowMark, IconFolder, IconHome, IconMore, IconSessions } from './components/icons';
import { HomeScreen } from './screens/Home';
import { SessionsScreen } from './screens/Sessions';
import { FilesScreen } from './screens/Files';
import { MoreScreen } from './screens/More';
import { TerminalScreen } from './screens/Terminal';
import { UnlockScreen } from './screens/Unlock';
import { PairScreen } from './screens/Pair';
import { LockdownScreen } from './screens/Lockdown';

export type Tab = 'home' | 'sessions' | 'files' | 'more';

/** How often to re-check whether the PC is answering. */
const POLL_ONLINE_MS = 15_000;
const POLL_OFFLINE_MS = 3_000;

/** The default config used until the real one arrives (or while offline). */
const FALLBACK_CONFIG: WakeConfig = {
  method: 'smartplug',
  shortcuts: { on: 'PC On', off: 'PC Off', powerCycle: 'PC Power Cycle' },
  wakeTimeoutSec: 180,
  powerCycleAfterSec: 90,
  offConfirmSec: 30,
  wakerUrl: null,
  routerHint: '',
};

/** Where the app is. A terminal view is a full-screen route, not a tab. */
type Route = { kind: 'tab'; tab: Tab } | { kind: 'terminal'; sessionId: string };

export function App() {
  const [reachable, setReachable] = useState<boolean | null>(null);
  const [authState, setAuthState] = useState<api.AuthState | null>(null);
  const [me, setMe] = useState<api.Me | null>(null);
  const [config, setConfig] = useState<WakeConfig>(FALLBACK_CONFIG);
  const [intent, setIntent] = useState<WakeIntent>(() => loadIntent(window.localStorage));
  const [now, setNow] = useState(Date.now());
  const [route, setRoute] = useState<Route>({ kind: 'tab', tab: 'home' });
  const [toasts, setToasts] = useState<Toast[]>([]);
  // Sticky while the pairing screen is up: the poll flips authState.paired to
  // true the moment the passkey is created, which used to unmount the screen
  // before its optional SSH-key step could be used.
  const [inPairing, setInPairing] = useState(false);

  const toastId = useRef(0);

  const toast = useCallback((text: string, kind: Toast['kind'] = 'info') => {
    const id = ++toastId.current;
    setToasts((t) => [...t, { id, text, kind }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 4200);
  }, []);

  /** Report an error in a way that is useful rather than alarming. */
  const reportError = useCallback(
    (e: unknown, fallback = 'That did not work') => {
      if (e instanceof passkey.PasskeyCancelled) return; // the user said no
      if (e instanceof ApiError) {
        toast(e.message || fallback, 'error');
        return;
      }
      if (e instanceof api.OfflineError) {
        toast('The PC did not answer', 'error');
        return;
      }
      toast(fallback, 'error');
    },
    [toast],
  );

  /** Persist the intent whenever it changes, so a reboot does not lose it. */
  const updateIntent = useCallback((next: WakeIntent) => {
    setIntent(next);
    saveIntent(window.localStorage, next);
  }, []);

  // ---- the single connectivity loop ----

  const pollRef = useRef<number | undefined>(undefined);

  // Focus and visibilitychange both fire on returning to the app (often
  // together), and each used to run a full check of four requests; a burst of
  // them tripped the rate limit and the app reported the PC as off. Checks are
  // coalesced: never two at once, and none within 2s of the last.
  const checkInFlight = useRef(false);
  const lastCheckAt = useRef(0);
  const missedProbes = useRef(0);
  const wasReachable = useRef(false);

  const check = useCallback(async () => {
    if (checkInFlight.current || Date.now() - lastCheckAt.current < 2000) return;
    checkInFlight.current = true;
    lastCheckAt.current = Date.now();
    try {
      await runCheck();
    } finally {
      checkInFlight.current = false;
    }
  }, []);

  const runCheck = async () => {
    const h = await api.health();
    const healthy = h !== null;
    const at = Date.now();

    // One missed probe is not "the PC is off": phones drop a request when
    // they wake, change networks, or while Tailscale reconnects, and flipping
    // the whole app to the Wake screen and back looked like constant
    // refreshing. While the PC was online, it takes two misses in a row; the
    // first is retried quickly instead of waiting for the next poll.
    if (healthy) {
      missedProbes.current = 0;
    } else if (wasReachable.current && ++missedProbes.current < 2) {
      window.setTimeout(() => void check(), 2500);
      return;
    }
    wasReachable.current = healthy;

    setReachable(healthy);
    setNow(at);
    // The one writer of the intent's health-derived fields.
    setIntent((prev) => {
      const next = applyHealth(prev, healthy, at);
      saveIntent(window.localStorage, next);
      return next;
    });

    if (healthy) {
      try {
        // The auth state is fetched on its own: wake-config needs a session,
        // so fetching both together threw the auth state away whenever the
        // device was signed out, and the Unlock screen never appeared.
        const state = await api.getAuthState();
        setAuthState(state);
        if (state.authenticated) {
          try {
            const [cfg, who] = await Promise.all([api.getWakeConfig(), api.getMe()]);
            setConfig(cfg);
            setMe(who);
          } catch {
            // Not fatal: the cached config and the phone default still work.
          }
        } else {
          setMe(null);
        }
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          // The gate answers 404 during lockdown and for an unknown device.
          setAuthState({
            paired: false,
            pairingOpen: false,
            authenticated: false,
            lockdown: true,
            maxDevices: 3,
          });
        }
      }
    }
  };

  // One check on open. The loop below only reschedules: it re-runs when
  // reachability flips (to change cadence), and checking again right then
  // doubled every request on each app open.
  useEffect(() => {
    void check();
  }, [check]);

  useEffect(() => {
    const schedule = () => {
      const period = reachable ? POLL_ONLINE_MS : POLL_OFFLINE_MS;
      pollRef.current = window.setTimeout(async () => {
        await check();
        schedule();
      }, period);
    };
    schedule();
    return () => {
      if (pollRef.current !== undefined) window.clearTimeout(pollRef.current);
    };
    // reachable is in the deps so the cadence changes when the PC goes away:
    // every 3s while waiting for a boot, every 15s once it is up.
  }, [check, reachable]);

  // A ticking clock, so the wake stepper counts up without a network call.
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);

  // Check immediately when the app comes back to the foreground: a phone that
  // was in a pocket should not show a stale "offline" for three seconds.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === 'visible') void check();
    };
    document.addEventListener('visibilitychange', onVisible);
    window.addEventListener('focus', onVisible);
    return () => {
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('focus', onVisible);
    };
  }, [check]);

  const deviceKind: DeviceKind = me?.device.kind ?? 'phone';

  const view: WakeView = useMemo(
    () =>
      wakeView({
        config,
        deviceKind,
        reachability: reachable ? 'online' : 'offline',
        intent,
        now,
      }),
    [config, deviceKind, reachable, intent, now],
  );

  // Give up on a wake that has run past its timeout, so the stepper does not
  // count forever and Wake becomes available again.
  useEffect(() => {
    if (wakeTimedOut({ config, deviceKind, reachability: 'offline', intent, now })) {
      updateIntent(clearIntent());
      toast('The PC did not come online. Try waking it again.', 'error');
    }
  }, [config, deviceKind, intent, now, toast, updateIntent]);

  // ---- actions the screens trigger ----

  const onWake = useCallback(() => {
    updateIntent(requestWake(intent, Date.now()));
    if (deviceKind === 'phone' && config.method === 'smartplug') {
      window.location.href = shortcutURL(config.shortcuts.on);
    }
  }, [config, deviceKind, intent, updateIntent]);

  const onPowerCycle = useCallback(() => {
    updateIntent(requestWake(clearIntent(), Date.now()));
    window.location.href = shortcutURL(config.shortcuts.powerCycle);
  }, [config, updateIntent]);

  const onCutPower = useCallback(() => {
    window.location.href = shortcutURL(config.shortcuts.off);
    updateIntent(clearIntent());
  }, [config, updateIntent]);

  const onShutdownRequested = useCallback(() => {
    updateIntent(requestShutdown(intent, Date.now()));
  }, [intent, updateIntent]);

  const onDismissPowerPrompt = useCallback(() => {
    updateIntent(clearIntent());
  }, [updateIntent]);

  const refreshAuth = useCallback(async () => {
    try {
      setAuthState(await api.getAuthState());
      setMe(await api.getMe());
    } catch {
      // The unlock screen handles its own failures.
    }
  }, []);

  // ---- what to render ----

  // Offline and lockdown are decided before anything else, because neither can
  // show a normal screen and both need an explanation rather than a spinner.
  if (reachable === false || (reachable === null && authState === null)) {
    if (reachable === null) {
      return (
        <div class="shell">
          <div class="scroll center" style="padding-top:28vh">
            <div class="spin" style="margin:0 auto 16px" />
            <p class="dim">Checking…</p>
          </div>
        </div>
      );
    }
    return (
      <HomeScreen
        offline
        view={view}
        config={config}
        system={null}
        projects={[]}
        sessions={[]}
        onWake={onWake}
        onPowerCycle={onPowerCycle}
        onCutPower={onCutPower}
        onDismissPowerPrompt={onDismissPowerPrompt}
        onStartSession={async () => undefined}
        onOpenTerminal={() => undefined}
        onGoTo={() => undefined}
        toast={toast}
      />
    );
  }

  if (authState?.lockdown) return <LockdownScreen />;

  if (authState && !authState.paired && !inPairing) setInPairing(true);
  if (authState && (!authState.paired || inPairing)) {
    return (
      <PairScreen
        pairingOpen={authState.pairingOpen}
        onPaired={() => {
          setInPairing(false);
          void refreshAuth();
        }}
        reportError={reportError}
        toast={toast}
      />
    );
  }

  if (authState && !authState.authenticated) {
    return <UnlockScreen onUnlocked={refreshAuth} reportError={reportError} />;
  }

  if (route.kind === 'terminal') {
    return (
      <TerminalScreen
        sessionId={route.sessionId}
        onBack={() => setRoute({ kind: 'tab', tab: 'sessions' })}
        reportError={reportError}
      />
    );
  }

  return (
    <Shell
      tab={route.tab}
      onTab={(tab) => setRoute({ kind: 'tab', tab })}
      status={view.phase === 'online' ? 'online' : view.phase === 'offline' ? 'offline' : 'waking'}
      statusLabel={view.message || 'Online'}
      toasts={toasts}
    >
      {route.tab === 'home' && (
        <HomeScreenConnected
          view={view}
          config={config}
          onWake={onWake}
          onPowerCycle={onPowerCycle}
          onCutPower={onCutPower}
          onDismissPowerPrompt={onDismissPowerPrompt}
          onOpenTerminal={(id) => setRoute({ kind: 'terminal', sessionId: id })}
          onGoTo={(tab) => setRoute({ kind: 'tab', tab })}
          toast={toast}
          reportError={reportError}
        />
      )}
      {route.tab === 'sessions' && (
        <SessionsScreen
          onOpenTerminal={(id) => setRoute({ kind: 'terminal', sessionId: id })}
          toast={toast}
          reportError={reportError}
        />
      )}
      {route.tab === 'files' && <FilesScreen toast={toast} reportError={reportError} />}
      {route.tab === 'more' && (
        <MoreScreen
          me={me}
          config={config}
          onShutdownRequested={onShutdownRequested}
          toast={toast}
          reportError={reportError}
        />
      )}
    </Shell>
  );
}

/**
 * HomeScreen needs live data; this wrapper owns the fetching so the screen
 * itself stays a pure render of what it is given.
 */
function HomeScreenConnected(props: {
  view: WakeView;
  config: WakeConfig;
  onWake: () => void;
  onPowerCycle: () => void;
  onCutPower: () => void;
  onDismissPowerPrompt: () => void;
  onOpenTerminal: (id: string) => void;
  onGoTo: (tab: Tab) => void;
  toast: (text: string, kind?: Toast['kind']) => void;
  reportError: (e: unknown, fallback?: string) => void;
}) {
  const [system, setSystem] = useState<api.SystemInfo | null>(null);
  const [projects, setProjects] = useState<api.Project[]>([]);
  const [sessions, setSessions] = useState<api.Session[]>([]);

  const { reportError } = props;
  // Depend on reportError, not props: props is a new object on every parent
  // render (the shell ticks once a second), which would re-fetch every second.
  const refresh = useCallback(async () => {
    try {
      const [sys, projs, sess] = await Promise.all([
        api.getSystem(),
        api.getProjects(),
        api.getSessions(),
      ]);
      setSystem(sys);
      setProjects(projs);
      setSessions(sess);
    } catch (e) {
      if (e instanceof api.OfflineError) return; // the shell already shows it
      reportError(e, 'Could not load the status');
    }
  }, [reportError]);

  useEffect(() => {
    void refresh();
    const id = window.setInterval(() => void refresh(), 10_000);
    return () => window.clearInterval(id);
  }, [refresh]);

  const startSession = useCallback(
    async (body: api.CreateSessionRequest) => {
      const created = await api.createSession(body);
      await refresh();
      return created;
    },
    [refresh],
  );

  return (
    <HomeScreen
      offline={false}
      view={props.view}
      config={props.config}
      system={system}
      projects={projects}
      sessions={sessions}
      onWake={props.onWake}
      onPowerCycle={props.onPowerCycle}
      onCutPower={props.onCutPower}
      onDismissPowerPrompt={props.onDismissPowerPrompt}
      onStartSession={startSession}
      onProjectsChanged={refresh}
      onOpenTerminal={props.onOpenTerminal}
      onGoTo={props.onGoTo}
      toast={props.toast}
    />
  );
}

/** The chrome: status pill, scroll area and tabs. */
function Shell({
  tab,
  onTab,
  status,
  statusLabel,
  toasts,
  children,
}: {
  tab: Tab;
  onTab: (t: Tab) => void;
  status: 'online' | 'offline' | 'waking';
  statusLabel: string;
  toasts: Toast[];
  children: preact.ComponentChildren;
}) {
  const tabs: { id: Tab; icon: preact.JSX.Element; label: string }[] = [
    { id: 'home', icon: <IconHome />, label: 'Home' },
    { id: 'sessions', icon: <IconSessions />, label: 'Sessions' },
    { id: 'files', icon: <IconFolder />, label: 'Files' },
    { id: 'more', icon: <IconMore />, label: 'More' },
  ];

  // Keyboard shortcuts for the wide layout (docs/05-UI.md).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      if (target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === '/') {
        e.preventDefault();
        onTab('files');
      }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onTab]);

  return (
    <div class="shell">
      <nav class="tabbar" aria-label="Main">
        {tabs.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => onTab(t.id)}
            aria-current={tab === t.id ? 'page' : undefined}
          >
            {t.icon}
            {t.label}
          </button>
        ))}
      </nav>

      <div class="main">
        <div class="statusbar">
          <span class="brand">
            <BurrowMark size={26} />
            Burrow
          </span>
          <span class="spacer" />
          <span class="pill">
            <span class={`dot ${status}`} aria-hidden="true" />
            {statusLabel}
          </span>
        </div>
        <div class="scroll">
          <div class="scroll-inner">{children}</div>
        </div>
      </div>

      <Toasts toasts={toasts} />
    </div>
  );
}
