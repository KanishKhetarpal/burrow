import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import * as passkey from '../passkey';
import { ApiError } from '../api';
import { BurrowMark, IconLock } from '../components/icons';

/** The unlock screen: one big Face ID button, auto-triggered once on open. */
export function UnlockScreen({
  onUnlocked,
  reportError,
}: {
  onUnlocked: () => void;
  reportError: (e: unknown, fallback?: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [lockedOut, setLockedOut] = useState<string | null>(null);
  const tried = useRef(false);

  const unlock = useCallback(async () => {
    if (busy) return;
    setBusy(true);
    try {
      await passkey.login();
      onUnlocked();
    } catch (e) {
      if (e instanceof ApiError && e.code === 'locked_out') {
        setLockedOut(e.message);
      } else {
        reportError(e, 'Could not unlock');
      }
    } finally {
      setBusy(false);
    }
  }, [busy, onUnlocked, reportError]);

  // Auto-trigger once, so opening the app from the home screen goes straight
  // to Face ID rather than needing a tap first (docs/07-TEST-PLAN.md M1).
  useEffect(() => {
    if (tried.current || !passkey.passkeysAvailable()) return;
    tried.current = true;
    void unlock();
  }, [unlock]);

  return (
    <div class="shell">
      <div class="scroll auth">
        <BurrowMark size={76} />
        <h1>Burrow</h1>
        {lockedOut ? (
          <>
            <p class="dim">Too many failed attempts.</p>
            <p class="faint small">{lockedOut}</p>
          </>
        ) : (
          <>
            <p class="dim" style="margin-bottom:28px">
              Your PC, from anywhere. Unlock with Face ID to continue.
            </p>
            <button
              type="button"
              class="btn-primary btn-big btn-block"
              onClick={() => void unlock()}
              disabled={busy}
              style="max-width:340px;margin:0 auto"
            >
              <IconLock size={20} />
              {busy ? 'Waiting…' : 'Unlock'}
            </button>
            {!passkey.passkeysAvailable() && (
              <p class="small faint" style="margin-top:20px">
                This browser cannot use passkeys. Open Burrow from the home-screen app.
              </p>
            )}
          </>
        )}
      </div>
    </div>
  );
}
