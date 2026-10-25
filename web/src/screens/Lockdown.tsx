import { IconShield } from '../components/icons';

/**
 * Lockdown. The agent answers 404 to everything but /healthz, so there is
 * nothing to show and nothing to do from here by design: clearing it needs a
 * console on the PC (docs/03-SECURITY.md §4).
 */
export function LockdownScreen() {
  return (
    <div class="shell">
      <div class="scroll auth">
        <span class="tile-ico red" style="width:76px;height:76px;border-radius:24px;margin-bottom:18px">
          <IconShield size={38} />
        </span>
        <h1>Locked down</h1>
        <p class="dim">Every session was stopped and all access is refused.</p>
        <div class="card">
          <p class="small dim" style="margin:0">
            Unlock it at the PC with <span class="mono">burrow unlock</span>.
          </p>
        </div>
      </div>
    </div>
  );
}
