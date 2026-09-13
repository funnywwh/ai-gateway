// The sidebar brand: the product name plus the build identity of the server behind this
// console (version a.b.c and short revision), which is how an operator tells at a glance
// which release they are looking at.
//
// It lives in its own module so the browser harness can render it directly: the shell it
// belongs to needs a session and a router, while this needs nothing but a version lookup.
import { el } from './ui.js';

/**
 * renderBrand builds the brand block. `lookup` returns a promise of
 * `{version, revision}`; it is called once and a rejection or null answer simply leaves
 * the badge empty — a console that cannot read its own version still has to work.
 */
export function renderBrand(lookup) {
  const box = el('div', { class: 'brand' }, [el('span', { text: 'AI Gateway' })]);
  Promise.resolve()
    .then(lookup)
    .then((info) => {
      if (!info) return;
      const text = (value) => (typeof value === 'string' ? value.trim() : '');
      const version = text(info.version);
      const revision = text(info.revision);
      // "none" is what an unversioned build reports; printing it as a revision would
      // suggest a commit that does not exist.
      const parts = [
        version ? el('span', { class: 'brand-version', text: 'v' + version }) : null,
        revision && revision !== 'none' ? el('span', { class: 'brand-revision', text: revision }) : null,
      ].filter(Boolean);
      if (parts.length) box.append(el('span', { class: 'brand-build' }, parts));
    })
    .catch(() => { /* the badge is decoration; never let it break the shell */ });
  return box;
}
