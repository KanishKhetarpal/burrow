import { useCallback, useEffect, useState } from 'preact/hooks';
import * as api from '../api';
import { StatusDot, timeAgo, type Toast } from '../components/ui';
import { IconExternal, IconPlay, IconPlus, IconSessions, IconStop, IconTerminal, IconTrash } from '../components/icons';
import { NewChatSheet } from './Home';

export function SessionsScreen(props: {
  onOpenTerminal: (id: string) => void;
  toast: (text: string, kind?: Toast['kind']) => void;
  reportError: (e: unknown, fallback?: string) => void;
}) {
  const [sessions, setSessions] = useState<api.Session[]>([]);
  const [busy, setBusy] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [projects, setProjects] = useState<api.Project[]>([]);

  const loadProjects = useCallback(async () => {
    try {
      setProjects(await api.getProjects());
    } catch {
      // The sheet still opens; it just has no projects to offer.
    }
  }, []);

  const { reportError } = props;
  // Depend on reportError, not props: props is a new object on every parent
  // render (the shell ticks once a second), which would re-fetch every second.
  const refresh = useCallback(async () => {
    try {
      setSessions(await api.getSessions());
    } catch (e) {
      if (!(e instanceof api.OfflineError)) reportError(e, 'Could not list sessions');
    }
  }, [reportError]);

  useEffect(() => {
    void refresh();
    const id = window.setInterval(() => void refresh(), 5000);
    return () => window.clearInterval(id);
  }, [refresh]);

  async function act(id: string, what: 'stop' | 'resume' | 'delete') {
    setBusy(id);
    try {
      if (what === 'stop') await api.stopSession(id);
      if (what === 'resume') await api.resumeSession(id);
      if (what === 'delete') await api.deleteSession(id);
      await refresh();
    } catch (e) {
      props.reportError(e, `Could not ${what} the session`);
    } finally {
      setBusy(null);
    }
  }

  async function reply(id: string, text: string) {
    try {
      await api.sendSessionText(id, text.endsWith('\r') ? text : `${text}\r`);
      await refresh();
    } catch (e) {
      props.reportError(e, 'Could not send that');
    }
  }

  const running = sessions.filter((s) => s.endedAt === null);

  return (
    <>
      <div class="page-head">
        <h1>Sessions</h1>
        {running.length > 0 && (
          <button
            type="button"
            class="btn-danger"
            style="min-height:38px;padding:6px 14px"
            onClick={async () => {
              if (!confirm(`Stop all ${running.length} sessions?`)) return;
              try {
                const r = await api.killAllSessions();
                props.toast(`Stopped ${r.killed}`, 'ok');
                await refresh();
              } catch (e) {
                props.reportError(e);
              }
            }}
          >
            Stop all
          </button>
        )}
        <button
          type="button"
          class="btn-primary fab"
          aria-label="New session"
          onClick={() => {
            void loadProjects();
            setCreating(true);
          }}
        >
          <IconPlus />
        </button>
      </div>

      {sessions.length === 0 && (
        <div class="empty">
          <span class="tile-ico gold">
            <IconSessions size={28} />
          </span>
          <div>
            <div style="font-weight:650;color:var(--text)">No sessions yet</div>
            <div class="small">Start Claude Code on your PC from here.</div>
          </div>
          <button
            type="button"
            class="btn-primary"
            onClick={() => {
              void loadProjects();
              setCreating(true);
            }}
          >
            <IconPlus size={18} />
            New session
          </button>
        </div>
      )}

      {sessions.map((s) => (
        <div key={s.id} class="card">
          <div style="display:flex;align-items:center;gap:12px">
            <span class={`tile-ico ${s.endedAt === null ? 'green' : ''}`}>
              <IconTerminal size={20} />
            </span>
            <div class="grow" style="flex:1;min-width:0">
              <div class="truncate" style="font-weight:650">
                {s.name}
              </div>
              <div class="tiny faint" style="display:flex;align-items:center;gap:6px;flex-wrap:wrap">
                <StatusDot status={s.status} label={s.status} />
                {s.status.replace('_', ' ')} · {s.mode === 'server' ? 'server' : 'session'} ·{' '}
                {timeAgo(s.startedAt)}
                {s.restarts > 0 && ` · ${s.restarts} restart${s.restarts === 1 ? '' : 's'}`}
              </div>
            </div>
          </div>

          {s.prompt && (
            <div class="banner warn" style="margin:12px 0 0;text-align:left">
              <p class="small" style="margin-bottom:8px">
                {s.prompt.text}
              </p>
              <div class="btn-row">
                {(s.prompt.options ?? ['y', 'n']).map((opt) => (
                  <button
                    key={opt}
                    type="button"
                    style="min-height:38px;padding:6px 14px"
                    onClick={() => void reply(s.id, opt)}
                  >
                    {labelForOption(s.prompt!.kind, opt)}
                  </button>
                ))}
              </div>
            </div>
          )}

          <div class="btn-row" style="margin-top:12px">
            {s.remoteUrl && (
              <a class="btn btn-primary" href={s.remoteUrl} rel="noreferrer">
                <IconExternal size={18} />
                Claude app
              </a>
            )}
            <button type="button" onClick={() => props.onOpenTerminal(s.id)}>
              <IconTerminal size={18} />
              Terminal
            </button>
            {s.endedAt === null ? (
              <button type="button" disabled={busy === s.id} onClick={() => void act(s.id, 'stop')}>
                <IconStop size={18} />
                Stop
              </button>
            ) : (
              <>
                <button
                  type="button"
                  disabled={busy === s.id}
                  onClick={() => void act(s.id, 'resume')}
                >
                  <IconPlay size={18} />
                  Resume
                </button>
                <button
                  type="button"
                  class="btn-danger"
                  disabled={busy === s.id}
                  onClick={() => void act(s.id, 'delete')}
                >
                  <IconTrash size={18} />
                  Delete
                </button>
              </>
            )}
          </div>
        </div>
      ))}

      {creating && (
        <NewChatSheet
          projects={projects}
          initialProject=""
          onClose={() => setCreating(false)}
          onStart={async (body) => {
            const s = await api.createSession(body);
            await refresh();
            return s;
          }}
          onProjectsChanged={loadProjects}
          onOpenTerminal={props.onOpenTerminal}
          toast={props.toast}
        />
      )}
    </>
  );
}

/** Turn a raw option into something readable on a button. */
function labelForOption(kind: string, opt: string): string {
  if (kind === 'trust' && opt === 'y') return 'Trust folder';
  if (kind === 'enable_rc' && opt === 'y') return 'Enable';
  if (opt === 'y') return 'Yes';
  if (opt === 'n') return 'No';
  return opt;
}
