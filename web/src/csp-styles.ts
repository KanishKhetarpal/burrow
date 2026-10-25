/**
 * Lets a library that injects <style> elements work under `style-src 'self'`.
 *
 * xterm's DOM renderer writes its cell layout and colour theme into <style>
 * elements it creates inside the terminal. The CSP (docs/03-SECURITY.md §4)
 * refuses inline style, so those sheets never apply and the terminal renders
 * with no colours and broken cell geometry — and nothing errors except a CSP
 * report in the console.
 *
 * Constructable stylesheets are not inline style: CSP does not govern CSSOM.
 * So rather than weakening the policy, this mirrors the text of every <style>
 * element under `root` into an adopted CSSStyleSheet and keeps it in sync as
 * the library rewrites it (xterm does on every resize and font change).
 *
 * Returns a function that stops observing and removes the adopted sheets.
 */
export function bridgeInjectedStyles(root: HTMLElement): () => void {
  const sheets = new Map<HTMLStyleElement, CSSStyleSheet>();

  const adopt = (sheet: CSSStyleSheet) => {
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
  };
  const drop = (sheet: CSSStyleSheet) => {
    document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => s !== sheet);
  };

  const sync = () => {
    const live = new Set(root.querySelectorAll('style'));
    for (const el of live) {
      let sheet = sheets.get(el);
      if (!sheet) {
        sheet = new CSSStyleSheet();
        sheets.set(el, sheet);
        adopt(sheet);
      }
      sheet.replaceSync(el.textContent ?? '');
    }
    for (const [el, sheet] of sheets) {
      if (!live.has(el)) {
        drop(sheet);
        sheets.delete(el);
      }
    }
  };

  // Setting textContent replaces the style element's text node, which is a
  // childList change in the subtree; characterData covers in-place edits.
  const observer = new MutationObserver(sync);
  observer.observe(root, { subtree: true, childList: true, characterData: true });
  sync();

  return () => {
    observer.disconnect();
    for (const sheet of sheets.values()) drop(sheet);
    sheets.clear();
  };
}
