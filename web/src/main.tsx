import { render } from 'preact';
import { App } from './app';
import './styles.css';

const root = document.getElementById('app');
if (root) render(<App />, root);

// The worker is what makes the app open with the PC off, so registration
// failing is worth knowing about but must never stop the app rendering.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js').catch((err: unknown) => {
      console.warn('Burrow: the offline worker did not register', err);
    });
  });
}
