import { useState } from 'preact/hooks';
import * as api from '../api';
import * as passkey from '../passkey';
import type { Toast } from '../components/ui';
import { BurrowMark } from '../components/icons';

/**
 * First-run pairing. The six-digit code is shown on the PC by `burrow pair`
 * and typed here; five wrong tries close the window at the server.
 */
export function PairScreen({
  pairingOpen,
  onPaired,
  reportError,
  toast,
}: {
  pairingOpen: boolean;
  onPaired: () => void;
  reportError: (e: unknown, fallback?: string) => void;
  toast: (text: string, kind?: Toast['kind']) => void;
}) {
  const [kind, setKind] = useState<'phone' | 'laptop'>(() => passkey.guessDeviceKind());
  const [name, setName] = useState(() => passkey.guessDeviceName(passkey.guessDeviceKind()));
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [sshKey, setSSHKey] = useState('');

  if (!pairingOpen && !done) {
    return (
      <div class="shell">
        <div class="scroll auth">
          <BurrowMark size={76} />
          <h1>Not paired</h1>
          <p class="dim">
            Run <span class="mono">burrow pair</span> on the PC, then reload this page.
          </p>
        </div>
      </div>
    );
  }

  async function submit(e: Event) {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    try {
      await passkey.pair(code.trim(), name.trim() || kind, kind);
      setDone(true);
      toast('Paired', 'ok');
    } catch (err) {
      reportError(err, 'Pairing failed');
      setCode('');
    } finally {
      setBusy(false);
    }
  }

  async function addKey() {
    if (!sshKey.trim()) {
      onPaired();
      return;
    }
    try {
      // Pairing normally leaves a fresh step-up; withStepUp covers the case
      // where it has lapsed while the key was being copied.
      const res = await passkey.withStepUp(() => api.addSSHKey(sshKey.trim()));
      toast(`Key added (${res.fingerprint.slice(0, 20)}…)`, 'ok');
    } catch (err) {
      reportError(err, 'Could not add the key');
      return;
    }
    onPaired();
  }

  return (
    <div class="shell">
      <div class="scroll">
        <div class="scroll-inner">
          {!done ? (
            <>
              <div class="auth" style="min-height:auto;padding-bottom:0">
                <BurrowMark size={64} />
                <h1>Pair this device</h1>
                <p class="dim">Enter the 6-digit code shown on your PC.</p>
              </div>
              <form class="card" onSubmit={submit}>
                <div class="field">
                  <label for="pair-code">Code</label>
                  <input
                    id="pair-code"
                    type="text"
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    maxLength={6}
                    value={code}
                    onInput={(e) => setCode((e.target as HTMLInputElement).value)}
                    style="font-size:1.5rem;letter-spacing:.3em;text-align:center"
                    required
                  />
                </div>
                <div class="field">
                  <label for="pair-name">Device name</label>
                  <input
                    id="pair-name"
                    type="text"
                    value={name}
                    onInput={(e) => setName((e.target as HTMLInputElement).value)}
                  />
                </div>
                <div class="field">
                  <label for="pair-kind">Device type</label>
                  <select
                    id="pair-kind"
                    value={kind}
                    onChange={(e) => {
                      const k = (e.target as HTMLSelectElement).value as 'phone' | 'laptop';
                      setKind(k);
                      setName(passkey.guessDeviceName(k));
                    }}
                  >
                    <option value="phone">Phone</option>
                    <option value="laptop">Laptop</option>
                  </select>
                </div>
                <button type="submit" class="btn-primary btn-block" disabled={busy || code.length !== 6}>
                  {busy ? 'Creating passkey…' : 'Create passkey'}
                </button>
              </form>
            </>
          ) : (
            <>
              <h1>Paired</h1>
              <div class="card">
                <h3>Add an SSH key (optional)</h3>
                <p class="small dim">
                  Paste the public key from Termius, or your laptop's
                  <span class="mono"> ~/.ssh/burrow_ed25519.pub</span>.
                </p>
                <textarea
                  value={sshKey}
                  placeholder="ssh-ed25519 AAAA… termius"
                  onInput={(e) => setSSHKey((e.target as HTMLTextAreaElement).value)}
                />
                <div class="btn-row" style="margin-top:12px">
                  <button type="button" class="btn-primary" onClick={() => void addKey()}>
                    {sshKey.trim() ? 'Add key and finish' : 'Finish'}
                  </button>
                </div>
              </div>
              <div class="card">
                <h3>Add to your Home Screen</h3>
                <p class="small dim">
                  Share → Add to Home Screen. Burrow then opens full-screen and can show the Wake
                  button even while the PC is off.
                </p>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
