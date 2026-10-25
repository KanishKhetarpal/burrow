/** Small shared pieces: toasts, sheets, meters, formatting. */

import type { ComponentChildren } from 'preact';
import { useEffect, useRef } from 'preact/hooks';
import { createPortal } from 'preact/compat';

/* ---------- formatting ---------- */

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.max(0, Math.round(seconds))}s`;
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/** "3m ago", for timestamps in lists. */
export function timeAgo(iso: string, now = Date.now()): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '';
  const secs = Math.max(0, (now - then) / 1000);
  if (secs < 45) return 'just now';
  return `${formatDuration(secs)} ago`;
}

export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

/* ---------- toasts ---------- */

export interface Toast {
  id: number;
  text: string;
  kind: 'ok' | 'error' | 'info';
}

export function Toasts({ toasts }: { toasts: Toast[] }) {
  if (toasts.length === 0) return null;
  return (
    // Announced politely so VoiceOver reads a result without interrupting.
    <div class="toasts" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} class={`toast ${t.kind}`}>
          {t.text}
        </div>
      ))}
    </div>
  );
}

/* ---------- sheet ---------- */

export function Sheet({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: ComponentChildren;
}) {
  const ref = useRef<HTMLDivElement>(null);
  // Callers pass an inline arrow, so onClose is a new function on every
  // render, and the shell re-renders every second. Keyed on it, the effect
  // below re-ran each second and its focus() pulled focus out of whatever
  // control was open — on iOS that snapped every <select> shut.
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    // Escape closes, and the sheet takes focus once, on open, so a screen
    // reader lands inside it rather than behind it.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCloseRef.current();
    };
    document.addEventListener('keydown', onKey);
    ref.current?.focus();
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  // Rendered into <body>: inside the scroll area, iOS Safari stacks a fixed
  // element under the tab bar however high its z-index, which hid Start.
  return createPortal(
    <div
      class="sheet-backdrop"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        class="sheet"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        ref={ref}
      >
        <div class="grabber" />
        <h3>{title}</h3>
        {children}
      </div>
    </div>,
    document.body,
  );
}

/* ---------- meters and status ---------- */

export function Meter({ used, total, label }: { used: number; total: number; label: string }) {
  const pct = total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0;
  return (
    <div>
      <div class="small dim" style="display:flex;justify-content:space-between;margin-bottom:4px">
        <span>{label}</span>
        <span>{pct}%</span>
      </div>
      <div
        class={`meter${pct >= 90 ? ' warn' : ''}`}
        role="meter"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={label}
      >
        <span style={`width:${pct}%`} />
      </div>
    </div>
  );
}

export function StatusDot({ status, label }: { status: string; label?: string }) {
  return (
    <>
      <span class={`dot ${status}`} aria-hidden="true" />
      {label !== undefined && <span class="sr-only">{label}</span>}
    </>
  );
}

export function Spinner({ label }: { label: string }) {
  return (
    <span style="display:inline-flex;align-items:center;gap:8px">
      <span class="spin" aria-hidden="true" />
      <span class="sr-only">{label}</span>
    </span>
  );
}

export function Empty({ children }: { children: ComponentChildren }) {
  return <div class="empty">{children}</div>;
}
