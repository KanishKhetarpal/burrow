import { useCallback, useEffect, useState } from 'preact/hooks';
import * as api from '../api';
import { withStepUp } from '../passkey';
import type { WakeConfig } from '../wake';
import { formatTime, timeAgo, type Toast } from '../components/ui';
import {
  IconChevronRight,
  IconCoffee,
  IconKey,
  IconLaptop,
  IconList,
  IconLock,
  IconMoon,
  IconPhone,
  IconPower,
  IconRestart,
} from '../components/icons';

export function MoreScreen(props: {
  me: api.Me | null;
  config: WakeConfig;
  onShutdownRequested: () => void;
  toast: (text: string, kind?: Toast['kind']) => void;
  reportError: (e: unknown, fallback?: string) => void;
}) {
  const [system, setSystem] = useState<api.SystemInfo | null>(null);
  const [devices, setDevices] = useState<api.Device[]>([]);
  const [audit, setAudit] = useState<api.AuditEntry[]>([]);
  const [showAudit, setShowAudit] = useState(false);
  const [confirmLockdown, setConfirmLockdown] = useState(0);
  const [showKey, setShowKey] = useState(false);
  const [sshKey, setSSHKey] = useState('');

  const { reportError } = props;
  // Depend on reportError, not props: props is a new object on every parent
  // render (the shell ticks once a second), which would re-fetch every second.
  const refresh = useCallback(async () => {
    try {
      const [sys, devs] = await Promise.all([api.getSystem(), api.getDevices()]);
      setSystem(sys);
      setDevices(devs);
    } catch (e) {
      if (!(e instanceof api.OfflineError)) reportError(e, 'Could not load settings');
    }
  }, [reportError]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  async function doPower(action: api.PowerAction) {
    const wakeable = action === 'shutdown' || action === 'restart' || action === 'lock';
    const warn =
      action === 'sleep' || action === 'hibernate'
        ? '\n\nNote: a sleeping PC cannot be woken from your phone.'
        : '';
    if (!confirm(`${action[0]!.toUpperCase()}${action.slice(1)} the PC?${warn}`)) return;

    try {
      // Step-up is required for shutdown and restart; this runs Face ID and
      // retries. A 409 means sessions are running, which is asked about rather
      // than forced.
      let res: api.PowerResult;
      try {
        res = await withStepUp(() => api.power(action));
      } catch (e) {
        if (e instanceof api.ApiError && e.code === 'sessions_running') {
          if (!confirm('Claude sessions are running. Continue anyway?')) return;
          res = await withStepUp(() => api.power(action, 0, true));
        } else {
          throw e;
        }
      }
      if (res.plugOffSafe) {
        // Hands the shutdown over to the wake machine, which will offer to cut
        // plug power once /healthz has failed continuously long enough.
        props.onShutdownRequested();
      }
      props.toast(`${action} requested`, 'ok');
      if (!wakeable) props.toast('This PC cannot be woken from your phone in this state', 'info');
    } catch (e) {
      props.reportError(e, `Could not ${action}`);
    }
  }

  async function setKeepAwake(minutes: number) {
    try {
      const res = await api.setKeepAwake(minutes > 0 || minutes === 0, minutes);
      setSystem((s) => (s ? { ...s, keepAwake: res.keepAwake } : s));
      props.toast(minutes === 0 ? 'Staying awake until switched off' : `Staying awake ${minutes}m`, 'ok');
    } catch (e) {
      props.reportError(e);
    }
  }

  async function loadAudit() {
    try {
      const res = await api.getAudit(200);
      setAudit(res.entries);
      setShowAudit(true);
    } catch (e) {
      props.reportError(e, 'Could not read the audit log');
    }
  }

  async function doLockdown() {
    try {
      const res = await withStepUp(() => api.lockdown());
      if (res.couldNotStop && res.couldNotStop.length > 0) {
        props.toast(`Locked down, but could not stop: ${res.couldNotStop.join(', ')}`, 'error');
      }
    } catch (e) {
      props.reportError(e, 'Could not lock down');
    }
  }

  return (
    <>
      <div class="page-head">
        <h1>More</h1>
      </div>

      <h2>Power</h2>
      <div class="card flush">
        <button type="button" class="row" onClick={() => void doPower('lock')}>
          <span class="tile-ico blue">
            <IconLock size={19} />
          </span>
          <span class="grow">Lock screen</span>
        </button>
        <button type="button" class="row" onClick={() => void doPower('restart')}>
          <span class="tile-ico gold">
            <IconRestart size={19} />
          </span>
          <span class="grow">Restart</span>
          <span class="tiny faint">Face ID</span>
        </button>
        <button type="button" class="row" onClick={() => void doPower('shutdown')}>
          <span class="tile-ico red">
            <IconPower size={19} />
          </span>
          <span class="grow">Shut down</span>
          <span class="tiny faint">Face ID</span>
        </button>
        <button type="button" class="row" onClick={() => void doPower('sleep')}>
          <span class="tile-ico">
            <IconMoon size={19} />
          </span>
          <span class="grow">
            Sleep
            <span class="tiny faint" style="display:block">
              Can't be woken from your phone
            </span>
          </span>
        </button>
      </div>

      <h2>Keep awake</h2>
      <div class="card">
        <p class="small dim" style="display:flex;align-items:center;gap:10px">
          <span class={`tile-ico ${system?.keepAwake.active ? 'green' : ''}`}>
            <IconCoffee size={19} />
          </span>
          {system?.keepAwake.active
            ? `Active (${system.keepAwake.reason})${
                system.keepAwake.until ? ` until ${formatTime(system.keepAwake.until)}` : ''
              }`
            : 'Off'}
        </p>
        <div class="btn-row">
          {[30, 60, 120, 240].map((m) => (
            <button key={m} type="button" onClick={() => void setKeepAwake(m)}>
              {m >= 60 ? `${m / 60}h` : `${m}m`}
            </button>
          ))}
          <button type="button" onClick={() => void setKeepAwake(0)}>
            Until off
          </button>
        </div>
        <button
          type="button"
          class="btn-block"
          style="margin-top:8px"
          onClick={async () => {
            try {
              const res = await api.setKeepAwake(false);
              setSystem((s) => (s ? { ...s, keepAwake: res.keepAwake } : s));
            } catch (e) {
              props.reportError(e);
            }
          }}
        >
          Switch off
        </button>
      </div>

      <h2>Security</h2>
      <div class="card flush">
        {devices.map((d) => (
          <div key={d.id} class="row">
            <span class="tile-ico">{d.kind === 'phone' ? <IconPhone size={19} /> : <IconLaptop size={19} />}</span>
            <span class="grow">
              {d.name}
              <span class="tiny faint" style="display:block">
                {d.kind} · {d.nodeTail}
                {d.current && ' · this device'}
              </span>
            </span>
            {!d.current && (
              <button
                type="button"
                class="btn-danger"
                style="min-height:36px;padding:6px 12px"
                onClick={async () => {
                  if (!confirm(`Revoke ${d.name}? It will lose access immediately.`)) return;
                  try {
                    await withStepUp(() => api.revokeDevice(d.id));
                    props.toast(`${d.name} revoked`, 'ok');
                    await refresh();
                  } catch (e) {
                    props.reportError(e);
                  }
                }}
              >
                Revoke
              </button>
            )}
          </div>
        ))}
        <button type="button" class="row" onClick={() => setShowKey((v) => !v)}>
          <span class="tile-ico gold">
            <IconKey size={19} />
          </span>
          <span class="grow">Add SSH key</span>
          <span class="chev">
            <IconChevronRight size={18} />
          </span>
        </button>
        <button type="button" class="row" onClick={() => void loadAudit()}>
          <span class="tile-ico">
            <IconList size={19} />
          </span>
          <span class="grow">Audit log</span>
          <span class="chev">
            <IconChevronRight size={18} />
          </span>
        </button>
      </div>

      {/* Pairing offers this once; this is the way back to it for a device that
          skipped it, or a new Termius key (docs/04-API.md /api/ssh-keys). */}
      {showKey && (
        <div class="card">
          <p class="small dim">
            Paste the public key from Termius (Keychain → your key → copy public key), or your
            laptop's <span class="mono">~/.ssh/burrow_ed25519.pub</span>.
          </p>
          <textarea
            rows={3}
            value={sshKey}
            placeholder="ssh-ed25519 AAAA… termius"
            onInput={(e) => setSSHKey((e.target as HTMLTextAreaElement).value)}
          />
          <button
            type="button"
            class="btn-primary btn-block"
            style="margin-top:8px"
            disabled={!sshKey.trim()}
            onClick={async () => {
              try {
                const res = await withStepUp(() => api.addSSHKey(sshKey.trim()));
                props.toast(`Key added (${res.fingerprint})`, 'ok');
                setSSHKey('');
                setShowKey(false);
              } catch (e) {
                props.reportError(e, 'Could not add the key');
              }
            }}
          >
            Add key
          </button>
        </div>
      )}

      {showAudit && (
        <div class="card flush" style="max-height:50vh;overflow:auto">
          {audit.map((e) => (
            <div key={e.seq} class="row" style="min-height:auto;padding:8px 14px">
              <span class="grow tiny">
                <span class="mono">{e.action}</span>
                {e.target && <span class="faint"> {e.target}</span>}
                <span class="faint" style="display:block">
                  {e.actor} · {timeAgo(e.ts)}
                  {e.result !== 'ok' && ` · ${e.result}`}
                </span>
              </span>
            </div>
          ))}
        </div>
      )}

      <h2>Panic</h2>
      <div class="card">
        <p class="small dim">
          Lockdown stops every session, refuses all access and stops SSH and the file share. It can
          only be cleared at the PC with <span class="mono">burrow unlock</span>.
        </p>
        <button
          type="button"
          class="btn-danger btn-block"
          onClick={() => {
            if (confirmLockdown === 0) {
              setConfirmLockdown(1);
              return;
            }
            void doLockdown();
          }}
        >
          {confirmLockdown === 0 ? 'Lock down…' : 'Tap again to lock down'}
        </button>
      </div>

      <h2>About</h2>
      <div class="card">
        <p class="tiny faint" style="line-height:1.8">
          Burrow {system?.version ?? ''}
          <br />
          {system?.hostname} · {system?.osVersion}
          <br />
          Signed in as {props.me?.login ?? '—'} on {props.me?.device.name ?? '—'}
          <br />
          Wake: {props.config.method}
          {props.config.method === 'smartplug' &&
            ` · ${props.config.shortcuts.on} / ${props.config.shortcuts.off} / ${props.config.shortcuts.powerCycle}`}
        </p>
        <button
          type="button"
          class="btn-block"
          style="margin-top:10px"
          onClick={async () => {
            try {
              await api.logout();
              location.reload();
            } catch (e) {
              props.reportError(e);
            }
          }}
        >
          Sign out
        </button>
      </div>
    </>
  );
}
