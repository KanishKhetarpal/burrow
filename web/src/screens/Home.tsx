import { useEffect, useState } from 'preact/hooks';
import * as api from '../api';
import { withStepUp } from '../passkey';
import type { WakeConfig, WakeView } from '../wake';
import type { Tab } from '../app';
import { Sheet, StatusDot, formatBytes, formatDuration, type Toast } from '../components/ui';
import {
  IconBriefcase,
  IconChevronRight,
  IconCpu,
  IconFolder,
  IconGlobe,
  IconPlus,
  IconSparkle,
  IconTerminal,
} from '../components/icons';

/**
 * Home. Pure render of what it is given, so the wake rules live in wake.ts and
 * are tested there rather than tangled up with fetching.
 */
export function HomeScreen(props: {
  offline: boolean;
  view: WakeView;
  config: WakeConfig;
  system: api.SystemInfo | null;
  projects: api.Project[];
  sessions: api.Session[];
  onWake: () => void;
  onPowerCycle: () => void;
  onCutPower: () => void;
  onDismissPowerPrompt: () => void;
  onStartSession: (body: api.CreateSessionRequest) => Promise<api.Session | undefined>;
  onProjectsChanged?: (() => Promise<void>) | undefined;
  onOpenTerminal: (id: string) => void;
  onGoTo: (tab: Tab) => void;
  toast: (text: string, kind?: Toast['kind']) => void;
}) {
  const { view } = props;
  const [newChat, setNewChat] = useState<string | null>(null);
  const [launcher, setLauncher] = useState<api.Launcher | null>(null);
  const [launchers, setLaunchers] = useState<api.Launcher[]>([]);
  const [launching, setLaunching] = useState<string | null>(null);
  const [pcApps, setPcApps] = useState<api.PcApp[]>([]);
  const [opening, setOpening] = useState<string | null>(null);

  async function openOnPc(name: string) {
    if (opening) return;
    setOpening(name);
    try {
      await api.openApp(name);
      props.toast(`Opened ${name} on your PC`, 'ok');
    } catch (e) {
      props.toast(e instanceof Error ? e.message : 'Could not open it', 'error');
    } finally {
      setOpening(null);
    }
  }

  // One tap: start Claude with the button's login in its folder and go
  // straight to the terminal, like typing claude1/claude2 in a terminal.
  async function launch(l: api.Launcher) {
    if (!l.oneTap) {
      setLauncher(l);
      setNewChat(l.projectId ?? '');
      return;
    }
    if (launching) return;
    setLaunching(l.name);
    try {
      // withStepUp only prompts if the server asks (a bypass launcher).
      const s = await withStepUp(() =>
        props.onStartSession({ launcher: l.name, projectId: l.projectId ?? '', mode: 'chat' }),
      );
      if (s) props.onOpenTerminal(s.id);
    } catch (e) {
      props.toast(e instanceof Error ? e.message : 'Could not start it', 'error');
    } finally {
      setLaunching(null);
    }
  }
  useEffect(() => {
    if (props.offline) return;
    api.getLaunchers().then(setLaunchers, () => undefined);
    api.getApps().then(setPcApps, () => undefined);
  }, [props.offline]);

  // The offline and shutdown states take over the whole screen: there is
  // nothing else useful to show, and the one action has to be obvious.
  if (props.offline || view.phase !== 'online') {
    return (
      <div class="shell">
        <div class="scroll">
          <div class="scroll-inner">
            <WakeCard {...props} />
          </div>
        </div>
      </div>
    );
  }

  const sys = props.system;
  const running = props.sessions.filter((s) => ['starting', 'ready', 'needs_input', 'running'].includes(s.status));
  const recent = props.projects.slice(0, 4);

  const pct = (used: number, total: number) => (total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0);
  const disk = sys?.disks[0];

  return (
    <>
      {sys && (
        <div class="card hero">
          <div class="hero-top">
            <span class="tile-ico green">
              <IconCpu />
            </span>
            <div class="grow" style="flex:1;min-width:0">
              <div class="hero-title truncate">{sys.hostname || 'Your PC'}</div>
              <div class="tiny faint">
                Up {formatDuration(sys.uptimeSec)}
                {sys.keepAwake.active && ' · staying awake'}
              </div>
            </div>
            <span class="pill">
              <span class="dot online" aria-hidden="true" />
              {running.length} running
            </span>
          </div>
          <div class="stat-grid">
            <Stat label="CPU" value={pct(sys.cpuPct, 100)} />
            <Stat label="Memory" value={pct(sys.ramUsedMB, sys.ramTotalMB)} />
            {disk && <Stat label={`Disk ${disk.mount}`} value={pct(disk.usedGB, disk.totalGB)} />}
          </div>
        </div>
      )}

      {launchers.length > 0 && (
        <>
          <h2>Quick start</h2>
          <div class="launch-grid">
            {launchers.map((l, i) => (
              <button
                key={l.name}
                type="button"
                class={`launch${i % 2 ? ' alt' : ''}`}
                disabled={launching !== null}
                onClick={() => void launch(l)}
              >
                <span class="ico">{i % 2 ? <IconBriefcase /> : <IconSparkle />}</span>
                <span>
                  <span class="name" style="display:block">
                    {launching === l.name ? 'Starting…' : l.name}
                  </span>
                  <span class="sub">{l.oneTap ? 'Opens a terminal' : 'Pick a project'}</span>
                </span>
              </button>
            ))}
          </div>
        </>
      )}

      <button
        type="button"
        class={launchers.length > 0 ? 'btn-big btn-block' : 'btn-primary btn-big btn-block'}
        onClick={() => {
          setLauncher(null);
          setNewChat('');
        }}
      >
        <IconPlus />
        New Claude Code session
      </button>

      <div class="quick-grid" style="margin-top:10px">
        {props.config.ssh && (
          <a class="btn" href={`ssh://${props.config.ssh.user}@${props.config.ssh.host}`} rel="noreferrer">
            <span class="tile-ico blue">
              <IconTerminal size={20} />
            </span>
            Terminal
          </a>
        )}
        <button type="button" onClick={() => props.onGoTo('files')}>
          <span class="tile-ico gold">
            <IconFolder size={20} />
          </span>
          Files
        </button>
      </div>

      {pcApps.length > 0 && (
        <>
          <h2>On your PC</h2>
          <div class="quick-grid">
            {pcApps.map((a) => (
              <button key={a.name} type="button" disabled={opening !== null} onClick={() => void openOnPc(a.name)}>
                <span class="tile-ico blue">{opening === a.name ? <span class="spin" /> : <IconGlobe size={20} />}</span>
                <span class="truncate">{a.name}</span>
              </button>
            ))}
          </div>
        </>
      )}

      {recent.length > 0 && (
        <>
          <h2>Start in a project</h2>
          <div class="chip-row">
            {recent.map((p) => (
              <button key={p.id} type="button" class="chip" onClick={() => setNewChat(p.id)}>
                {p.name}
              </button>
            ))}
          </div>
        </>
      )}

      {running.length > 0 && (
        <>
          <h2>Running</h2>
          <div class="card flush">
            {running.map((s) => (
              <button key={s.id} type="button" class="row" onClick={() => props.onOpenTerminal(s.id)}>
                <span class="tile-ico green">
                  <IconTerminal size={20} />
                </span>
                <span class="grow">
                  <span class="truncate" style="display:block;font-weight:600">
                    {s.name}
                  </span>
                  <span class="tiny faint" style="display:flex;align-items:center;gap:6px">
                    <StatusDot status={s.status} label={s.status} />
                    {s.status.replace('_', ' ')}
                  </span>
                </span>
                <span class="chev">
                  <IconChevronRight size={18} />
                </span>
              </button>
            ))}
          </div>
        </>
      )}

      {newChat !== null && (
        <NewChatSheet
          projects={props.projects}
          initialProject={newChat}
          launcher={launcher}
          onClose={() => {
            setNewChat(null);
            setLauncher(null);
          }}
          onStart={props.onStartSession}
          onProjectsChanged={props.onProjectsChanged}
          onOpenTerminal={props.onOpenTerminal}
          toast={props.toast}
        />
      )}
    </>
  );
}

/** The whole-screen wake / shutdown card. */
function WakeCard(props: {
  view: WakeView;
  config: WakeConfig;
  onWake: () => void;
  onPowerCycle: () => void;
  onCutPower: () => void;
  onDismissPowerPrompt: () => void;
}) {
  const { view } = props;
  const [confirmCycle, setConfirmCycle] = useState(false);

  return (
    <div style="min-height:70dvh;display:flex;flex-direction:column;justify-content:center;text-align:center">
      <h1>{view.phase === 'confirmed-off' ? 'PC is off' : 'Your PC'}</h1>
      <p class="dim" aria-live="polite">
        {view.message}
      </p>

      {(view.phase === 'waking' || view.phase === 'waking-stalled') && (
        <p class="stepper" style="justify-content:center;margin-bottom:18px">
          <span class="step done">Power on</span>
          <span aria-hidden="true">›</span>
          <span class={`step ${view.elapsedSec > 10 ? 'active' : ''}`}>Booting</span>
          <span aria-hidden="true">›</span>
          <span class="step">Online</span>
          <span class="faint">· {formatDuration(view.elapsedSec)}</span>
        </p>
      )}

      {view.canWake && !view.wakeNeedsPhone && (
        <button
          type="button"
          class="btn-primary btn-big btn-block"
          onClick={props.onWake}
          style="max-width:340px;margin:8px auto"
        >
          Wake PC
        </button>
      )}

      {view.wakeNeedsPhone && view.phase !== 'confirmed-off' && (
        <div class="banner info" style="max-width:420px;margin:16px auto">
          Wake from your iPhone: run the <strong>{props.config.shortcuts.on}</strong> shortcut, or
          say “Hey Siri, {props.config.shortcuts.on}”.
        </div>
      )}

      {view.canPowerCycle && (
        <div class="banner warn" style="max-width:420px;margin:16px auto;text-align:left">
          <p>
            <strong>Still off?</strong> The plug may have been left on, so turning it on again does
            nothing.
          </p>
          {!confirmCycle ? (
            <button type="button" onClick={() => setConfirmCycle(true)}>
              Power-cycle…
            </button>
          ) : (
            <>
              <p class="small">
                Only do this if the PC is fully shut down. It cuts power for 15 seconds, which would
                be a hard power-off if it is actually running.
              </p>
              <div class="btn-row">
                <button type="button" class="btn-danger" onClick={props.onPowerCycle}>
                  Cut power and restore
                </button>
                <button type="button" onClick={() => setConfirmCycle(false)}>
                  Cancel
                </button>
              </div>
            </>
          )}
        </div>
      )}

      {view.canCutPower && (
        <div class="card" style="max-width:420px;margin:16px auto;text-align:left">
          <h3>Cut plug power?</h3>
          <p class="small dim">
            The PC has been off for {formatDuration(view.elapsedSec)}. Turning the plug off saves
            power; the PC is woken by turning it back on.
          </p>
          <div class="btn-row">
            <button type="button" class="btn-primary" onClick={props.onCutPower}>
              Turn plug off
            </button>
            <button type="button" onClick={props.onDismissPowerPrompt}>
              Leave it on
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

/** The new-chat bottom sheet. */
export function NewChatSheet(props: {
  projects: api.Project[];
  initialProject: string;
  /** Started from a launcher button: its login, and straight to the terminal. */
  launcher?: api.Launcher | null;
  onClose: () => void;
  onStart: (body: api.CreateSessionRequest) => Promise<api.Session | undefined>;
  onProjectsChanged?: (() => Promise<void>) | undefined;
  onOpenTerminal: (id: string) => void;
  toast: (text: string, kind?: Toast['kind']) => void;
}) {
  const [adding, setAdding] = useState(false);
  const first = props.projects[0];
  const [projectId, setProjectId] = useState(props.initialProject || first?.id || '');
  // The Sessions screen opens the sheet before its project list arrives.
  useEffect(() => {
    if (!projectId && first) setProjectId(first.id);
  }, [projectId, first]);
  const [name, setName] = useState('');
  const [mode, setMode] = useState<'chat' | 'server'>('chat');
  type PermMode = 'default' | 'acceptEdits' | 'plan' | 'bypassPermissions';
  const [permissionMode, setPermissionMode] = useState<PermMode>(props.launcher?.permissionMode ?? 'default');
  const [bypassAllowed, setBypassAllowed] = useState(false);
  useEffect(() => {
    api.getMe().then((me) => setBypassAllowed(!!me.bypassPermissionsAllowed), () => undefined);
  }, []);
  const [spawn, setSpawn] = useState<'same-dir' | 'worktree'>('same-dir');
  const [busy, setBusy] = useState(false);
  const [started, setStarted] = useState<api.Session | null>(null);

  const project = props.projects.find((p) => p.id === projectId);

  async function start() {
    if (!projectId || busy) return;
    setBusy(true);
    try {
      const body: api.CreateSessionRequest = { projectId, mode, permissionMode };
      if (props.launcher) body.launcher = props.launcher.name;
      if (name.trim()) body.name = name.trim();
      if (mode === 'server') body.spawn = spawn;
      // A bypass session needs Face ID every time; the server asks for it.
      const s =
        permissionMode === 'bypassPermissions'
          ? await withStepUp(() => props.onStart(body))
          : await props.onStart(body);
      if (s && props.launcher) {
        // A launcher button means "give me a terminal": go straight there.
        props.onOpenTerminal(s.id);
        props.onClose();
        return;
      }
      if (s) setStarted(s);
    } catch (e) {
      props.toast(e instanceof Error ? e.message : 'Could not start the session', 'error');
    } finally {
      setBusy(false);
    }
  }

  if (started) {
    return (
      <Sheet title="Session started" onClose={props.onClose}>
        <p class="dim">{started.name}</p>
        {started.remoteUrl ? (
          <a class="btn btn-primary btn-block" href={started.remoteUrl} rel="noreferrer">
            Open in Claude
          </a>
        ) : (
          <>
            <p class="small dim">
              Waiting for the Remote Control link. If it does not appear, open the Claude app →
              Code tab; the session is listed as <strong>{started.name}</strong>.
            </p>
            <a class="btn btn-block" href="https://claude.ai/code" rel="noreferrer">
              Open Claude
            </a>
          </>
        )}
        <button
          type="button"
          class="btn-block"
          style="margin-top:8px"
          onClick={() => {
            props.onOpenTerminal(started.id);
            props.onClose();
          }}
        >
          View terminal
        </button>
      </Sheet>
    );
  }

  if (adding) {
    return (
      <AddProjectSheet
        onClose={() => setAdding(false)}
        onAdded={async (p) => {
          await props.onProjectsChanged?.();
          setProjectId(p.id);
          setAdding(false);
        }}
        toast={props.toast}
      />
    );
  }

  return (
    <Sheet title={props.launcher ? props.launcher.name : 'New Claude Code session'} onClose={props.onClose}>
      <div class="field">
        <div style="display:flex;align-items:baseline;justify-content:space-between">
          <label for="nc-project">Project</label>
          <button
            type="button"
            style="min-height:32px;padding:4px 10px"
            onClick={() => setAdding(true)}
          >
            + Add project
          </button>
        </div>
        <select
          id="nc-project"
          value={projectId}
          onChange={(e) => setProjectId((e.target as HTMLSelectElement).value)}
        >
          {props.projects.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
      </div>
      <div class="field">
        <label for="nc-name">Name</label>
        <input
          id="nc-name"
          type="text"
          placeholder={project ? `${project.name}-${new Date().toTimeString().slice(0, 5).replace(':', '')}` : ''}
          value={name}
          onInput={(e) => setName((e.target as HTMLInputElement).value)}
        />
      </div>
      <div class="field">
        <label for="nc-mode">Mode</label>
        <select
          id="nc-mode"
          value={mode}
          onChange={(e) => setMode((e.target as HTMLSelectElement).value as 'chat' | 'server')}
        >
          <option value="chat">Claude Code — one session</option>
          <option value="server">Claude Code — server (start more from the Claude app)</option>
        </select>
        <p class="tiny faint" style="margin-top:6px">
          Both run the real Claude Code CLI in this folder on your PC.
        </p>
      </div>
      {mode === 'server' && project?.isGit && (
        <div class="field switch">
          <label for="nc-worktree" style="margin:0">
            Use a git worktree
          </label>
          <input
            id="nc-worktree"
            type="checkbox"
            style="width:auto;min-height:auto"
            checked={spawn === 'worktree'}
            onChange={(e) =>
              setSpawn((e.target as HTMLInputElement).checked ? 'worktree' : 'same-dir')
            }
          />
        </div>
      )}
      <div class="field">
        <label for="nc-perm">Permission mode</label>
        <select
          id="nc-perm"
          value={permissionMode}
          onChange={(e) =>
            setPermissionMode((e.target as HTMLSelectElement).value as PermMode)
          }
        >
          {/* Offered only when the PC's burrow.yaml allows it, and each start
              needs Face ID (docs/DEVIATIONS.md D-8). */}
          <option value="default">Default</option>
          <option value="acceptEdits">Accept edits</option>
          <option value="plan">Plan</option>
          {bypassAllowed && <option value="bypassPermissions">Bypass permissions (no prompts)</option>}
        </select>
        {permissionMode === 'bypassPermissions' && (
          <p class="tiny" style="margin-top:6px;color:var(--warn)">
            Claude will run commands and edit files without asking. Face ID is needed to start it.
          </p>
        )}
      </div>
      <button
        type="button"
        class="btn-primary btn-block btn-big"
        onClick={() => void start()}
        disabled={busy || !projectId}
      >
        {busy ? 'Starting…' : 'Start'}
      </button>
    </Sheet>
  );
}

/** Pick a folder to add as a project; behind Face ID. */
function AddProjectSheet(props: {
  onClose: () => void;
  onAdded: (p: api.Project) => Promise<void>;
  toast: (text: string, kind?: Toast['kind']) => void;
}) {
  const [items, setItems] = useState<api.ProjectCandidate[] | null>(null);
  const [filter, setFilter] = useState('');
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    void (async () => {
      try {
        setItems(await withStepUp(() => api.getProjectCandidates()));
      } catch (e) {
        setError(e instanceof Error ? e.message : 'Could not list folders');
      }
    })();
  }, []);

  const shown = (items ?? []).filter((c) => c.name.toLowerCase().includes(filter.trim().toLowerCase()));

  return (
    <Sheet title="Add a project" onClose={props.onClose}>
      {error && <div class="banner warn">{error}</div>}
      {items === null && !error && <p class="dim">Loading folders…</p>}
      {items !== null && items.length === 0 && (
        <p class="small dim">
          No folders to offer. Add a parent folder (for example C:\dev) to
          <span class="mono"> projects_parents</span> in burrow.yaml on the PC.
        </p>
      )}
      {items !== null && items.length > 0 && (
        <>
          <input
            type="search"
            placeholder="Filter folders"
            value={filter}
            onInput={(e) => setFilter((e.target as HTMLInputElement).value)}
          />
          <div class="card flush" style="margin-top:10px;max-height:55dvh;overflow:auto">
            {shown.map((c) => (
              <button
                key={c.path}
                type="button"
                class="row"
                disabled={c.added || busy !== null}
                onClick={async () => {
                  setBusy(c.path);
                  try {
                    const p = await withStepUp(() => api.addProject(c.path));
                    props.toast(`Added ${p.name}`, 'ok');
                    await props.onAdded(p);
                  } catch (e) {
                    props.toast(e instanceof Error ? e.message : 'Could not add it', 'error');
                  } finally {
                    setBusy(null);
                  }
                }}
              >
                <span class="grow">
                  <span class="truncate" style="display:block">
                    {c.name}
                  </span>
                  <span class="tiny faint truncate" style="display:block">
                    {c.added ? 'already a project' : c.parent}
                    {c.isGit && ' · git'}
                  </span>
                </span>
                {busy === c.path && <span class="spin" />}
              </button>
            ))}
          </div>
        </>
      )}
    </Sheet>
  );
}

/** A system stat tile: label, big number, thin meter. */
function Stat({ label, value }: { label: string; value: number }) {
  return (
    <div class="stat">
      <div class="lbl">{label}</div>
      <div class="val">{value}%</div>
      <div
        class={`meter${value >= 90 ? ' warn' : ''}`}
        style="margin-top:8px"
        role="meter"
        aria-valuenow={value}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={label}
      >
        <span style={`width:${value}%`} />
      </div>
    </div>
  );
}

export { formatBytes };
