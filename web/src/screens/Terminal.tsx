import { useEffect, useRef, useState } from 'preact/hooks';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import * as api from '../api';
import { bridgeInjectedStyles } from '../csp-styles';

/** Keys the phone has no way to type, offered as a bar above the keyboard. */
const KEY_BAR: { label: string; key: string }[] = [
  { label: 'Esc', key: 'esc' },
  { label: 'Tab', key: 'tab' },
  { label: '^C', key: 'ctrl_c' },
  { label: '↑', key: 'up' },
  { label: '↓', key: 'down' },
  { label: '←', key: 'left' },
  { label: '→', key: 'right' },
  { label: 'y', key: 'y' },
  { label: 'n', key: 'n' },
  { label: '⏎', key: 'enter' },
];

/**
 * The raw terminal.
 *
 * Read-only by default: this view exists mostly to watch and to answer a
 * prompt, and a stray tap that types into a live Claude session is worse than
 * one extra tap to unlock typing (docs/05-UI.md §6).
 */
export function TerminalScreen(props: {
  sessionId: string;
  onBack: () => void;
  reportError: (e: unknown, fallback?: string) => void;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const readOnlyRef = useRef(true);

  const [readOnly, setReadOnly] = useState(true);
  const [status, setStatus] = useState('connecting…');
  const [fontSize, setFontSize] = useState(13);

  useEffect(() => {
    readOnlyRef.current = readOnly;
  }, [readOnly]);

  useEffect(() => {
    if (!hostRef.current) return;

    const term = new Terminal({
      fontSize,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      convertEol: false,
      cursorBlink: true,
      scrollback: 5000,
      theme: { background: '#000000', foreground: '#e8eaed' },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    // Before open(): xterm injects its theme and cell-layout <style> elements
    // as it opens, and the CSP would otherwise refuse them.
    const unbridge = bridgeInjectedStyles(hostRef.current);
    term.open(hostRef.current);
    fit.fit();
    termRef.current = term;

    let closed = false;
    let retry = 0;
    let reconnectTimer: number | undefined;

    const connect = () => {
      if (closed) return;
      const url = `${location.origin.replace(/^http/, 'ws')}/api/sessions/${props.sessionId}/term`;
      const ws = new WebSocket(url, 'burrow.term.v1');
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;

      ws.onopen = () => {
        retry = 0;
        setStatus('live');
        ws.send(JSON.stringify({ t: 'resize', cols: term.cols, rows: term.rows }));
      };
      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          try {
            const msg = JSON.parse(ev.data) as { t: string; status?: string; code?: number };
            if (msg.t === 'exit') setStatus(`exited (${msg.code ?? 0})`);
            else if (msg.status) setStatus(msg.status.replace('_', ' '));
          } catch {
            // A control frame we do not understand is not worth surfacing.
          }
          return;
        }
        term.write(new Uint8Array(ev.data as ArrayBuffer));
      };
      ws.onclose = () => {
        if (closed) return;
        setStatus('reconnecting…');
        // Backoff, capped: a session that has exited will never come back and
        // should not be hammered.
        retry = Math.min(retry + 1, 6);
        reconnectTimer = window.setTimeout(connect, 500 * 2 ** (retry - 1));
      };
      ws.onerror = () => ws.close();
    };

    // Keystrokes only travel when typing is unlocked.
    term.onData((data) => {
      if (readOnlyRef.current) return;
      wsRef.current?.send(new TextEncoder().encode(data));
    });

    const onResize = () => {
      fit.fit();
      wsRef.current?.send(JSON.stringify({ t: 'resize', cols: term.cols, rows: term.rows }));
    };
    window.addEventListener('resize', onResize);

    // A ping keeps the server's idle timer from closing a socket the user is
    // still watching.
    const ping = window.setInterval(() => {
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(JSON.stringify({ t: 'ping' }));
      }
    }, 120_000);

    connect();

    return () => {
      closed = true;
      window.clearInterval(ping);
      if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer);
      window.removeEventListener('resize', onResize);
      wsRef.current?.close();
      term.dispose();
      unbridge();
      termRef.current = null;
    };
  }, [props.sessionId, fontSize]);

  async function sendKey(key: string) {
    try {
      if (key.length === 1) await api.sendSessionText(props.sessionId, key);
      else await api.sendSessionKey(props.sessionId, key);
    } catch (e) {
      props.reportError(e, 'Could not send that key');
    }
  }

  return (
    <div class="shell">
      <div class="main" style="display:flex;flex-direction:column;min-height:0">
        <div class="statusbar">
          <button type="button" style="min-height:36px;padding:4px 10px" onClick={props.onBack}>
            ‹ Back
          </button>
          <span class="pill">{status}</span>
          <span class="spacer" />
          <button
            type="button"
            style="min-height:36px;padding:4px 10px"
            onClick={() => setFontSize((f) => Math.max(9, f - 1))}
            aria-label="Smaller text"
          >
            A−
          </button>
          <button
            type="button"
            style="min-height:36px;padding:4px 10px"
            onClick={() => setFontSize((f) => Math.min(22, f + 1))}
            aria-label="Larger text"
          >
            A+
          </button>
          <button
            type="button"
            class={readOnly ? '' : 'btn-primary'}
            style="min-height:36px;padding:4px 12px"
            onClick={() => {
              const next = !readOnly;
              readOnlyRef.current = next;
              setReadOnly(next);
              // Focus inside the tap itself: iOS only raises the keyboard for
              // a focus that happens during the user gesture, and leaving
              // focus on this button sends keystrokes (and Enter) to it.
              if (next) termRef.current?.blur();
              else termRef.current?.focus();
            }}
          >
            {readOnly ? 'Tap to type' : 'Typing'}
          </button>
        </div>

        <div class="term-wrap" ref={hostRef} />

        <div class="keybar">
          {KEY_BAR.map((k) => (
            <button key={k.key} type="button" onClick={() => void sendKey(k.key)}>
              {k.label}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
