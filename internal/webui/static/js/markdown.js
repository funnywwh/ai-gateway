// Markdown -> DOM.
//
// The console runs under a strict CSP (no inline scripts, no external resources), and a
// model's answer is untrusted input, so this renderer never sets innerHTML: every piece of
// text goes through textContent, and every attribute is validated. That is the whole reason
// there is a hand-written renderer instead of a library from a CDN — the console has no
// build step and loads nothing from the network.
//
// The dialect is the subset a model actually writes: headings, fenced code, GFM tables,
// ordered/unordered lists, blockquotes, horizontal rules, paragraphs, and inline code,
// emphasis, strikethrough and links. Anything unrecognised stays literal text, which is the
// honest outcome for markup this renderer does not understand.

const LINK_SCHEMES = ['http://', 'https://', 'mailto:'];

function text(value) { return document.createTextNode(value); }

function element(tag, className, children) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  for (const child of [].concat(children || [])) {
    if (child === null || child === undefined) continue;
    node.append(child instanceof Node ? child : text(String(child)));
  }
  return node;
}

// safeHref returns the href to use, or null when the target is not allowed. A model can
// emit javascript: or data: URLs; those become plain text instead of a live link.
export function safeHref(raw) {
  const value = String(raw || '').trim();
  if (!value) return null;
  const lower = value.toLowerCase();
  if (LINK_SCHEMES.some((scheme) => lower.startsWith(scheme))) return value;
  return null;
}

// ---------------------------------------------------------------------------
// inline
// ---------------------------------------------------------------------------

const INLINE = [
  { re: /`([^`]+)`/, make: (m) => element('code', 'md-code', [m[1]]) },
  { re: /\*\*([^*]+)\*\*/, make: (m) => element('strong', null, [m[1]]) },
  { re: /__([^_]+)__/, make: (m) => element('strong', null, [m[1]]) },
  { re: /~~([^~]+)~~/, make: (m) => element('del', null, [m[1]]) },
  { re: /\*([^*]+)\*/, make: (m) => element('em', null, [m[1]]) },
  { re: /\[([^\]]+)\]\(([^)\s]+)\)/, make: (m) => inlineLink(m[1], m[2]) },
];

function inlineLink(label, href) {
  const target = safeHref(href);
  if (!target) return element('span', 'md-link-blocked', [label + ' (' + href + ')']);
  return linkNode(label, target);
}

function linkNode(label, href) {
  const node = element('a', null, [label]);
  node.setAttribute('href', href);
  node.setAttribute('target', '_blank');
  node.setAttribute('rel', 'noopener noreferrer');
  return node;
}

// renderInline turns one line of text into nodes, honouring the inline rules in order of
// appearance so a later pattern cannot eat an earlier one.
export function renderInline(source) {
  const out = [];
  let rest = String(source === undefined || source === null ? '' : source);
  while (rest.length) {
    let best = null;
    for (const rule of INLINE) {
      const match = rule.re.exec(rest);
      if (match && (best === null || match.index < best.match.index)) {
        best = { match, rule };
      }
    }
    if (!best) {
      out.push(text(rest));
      break;
    }
    if (best.match.index > 0) out.push(text(rest.slice(0, best.match.index)));
    out.push(best.rule.make(best.match));
    rest = rest.slice(best.match.index + best.match[0].length);
  }
  return out;
}

// ---------------------------------------------------------------------------
// blocks
// ---------------------------------------------------------------------------

const FENCE = /^(\s*)(`{3,}|~{3,})\s*([^`]*)$/;
const HEADING = /^(#{1,6})\s+(.*)$/;
const HR = /^\s{0,3}(-{3,}|\*{3,}|_{3,})\s*$/;
const BULLET = /^(\s*)([-*+])\s+(.*)$/;
const ORDERED = /^(\s*)(\d+)[.)]\s+(.*)$/;
const QUOTE = /^\s*>\s?(.*)$/;
const TABLE_DIVIDER = /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$/;

function splitRow(line) {
  const trimmed = line.trim().replace(/^\|/, '').replace(/\|$/, '');
  return trimmed.split('|').map((cell) => cell.trim());
}

// parseBlocks splits the document into block descriptors. It is exported because the
// console also needs to find code blocks (to attach a preview toolbar) without walking DOM.
export function parseBlocks(source) {
  const lines = String(source || '').replace(/\r\n?/g, '\n').split('\n');
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i += 1; continue; }

    const fence = FENCE.exec(line);
    if (fence) {
      const marker = fence[2][0];
      const info = fence[3].trim();
      const body = [];
      i += 1;
      let closed = false;
      while (i < lines.length) {
        const candidate = lines[i];
        const close = FENCE.exec(candidate);
        if (close && close[2][0] === marker && close[3].trim() === '') { closed = true; i += 1; break; }
        body.push(candidate);
        i += 1;
      }
      blocks.push({ type: 'code', lang: info, text: body.join('\n'), closed });
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      blocks.push({ type: 'heading', level: heading[1].length, text: heading[2] });
      i += 1;
      continue;
    }

    if (HR.test(line)) { blocks.push({ type: 'hr' }); i += 1; continue; }

    if (QUOTE.test(line)) {
      const body = [];
      while (i < lines.length && QUOTE.test(lines[i])) {
        body.push(QUOTE.exec(lines[i])[1]);
        i += 1;
      }
      blocks.push({ type: 'quote', lines: body });
      continue;
    }

    // A table needs a header row, a divider and at least one body row.
    if (line.includes('|') && i + 1 < lines.length && TABLE_DIVIDER.test(lines[i + 1])) {
      const header = splitRow(line);
      const rows = [];
      i += 2;
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
        rows.push(splitRow(lines[i]));
        i += 1;
      }
      blocks.push({ type: 'table', header, rows });
      continue;
    }

    if (BULLET.test(line) || ORDERED.test(line)) {
      const items = [];
      while (i < lines.length && (BULLET.test(lines[i]) || ORDERED.test(lines[i]))) {
        const match = BULLET.exec(lines[i]) || ORDERED.exec(lines[i]);
        items.push(match[3]);
        i += 1;
      }
      blocks.push({ type: 'list', ordered: ORDERED.test(line), items });
      continue;
    }

    const paragraph = [];
    while (i < lines.length && lines[i].trim()
      && !FENCE.test(lines[i]) && !HEADING.test(lines[i]) && !HR.test(lines[i])
      && !QUOTE.test(lines[i]) && !BULLET.test(lines[i]) && !ORDERED.test(lines[i])) {
      paragraph.push(lines[i]);
      i += 1;
    }
    blocks.push({ type: 'paragraph', text: paragraph.join('\n') });
  }
  return blocks;
}

// renderMarkdown renders one document into a fragment. Code blocks carry data-lang and
// data-block-index so a page can attach "preview"/"export" toolbars without re-parsing, plus
// data-closed, which says whether the fence was terminated.
//
// data-closed exists because an answer streams: while the model is still writing, the last fence
// has no closing marker, so its body is a *prefix* of whatever it is writing. A renderer that
// tried to use that prefix would report "not valid JSON" for most of every answer — a fabricated
// error about the model, caused entirely by the reader looking too early.
export function renderMarkdown(source) {
  const fragment = document.createDocumentFragment();
  let codeIndex = 0;
  for (const block of parseBlocks(source)) {
    switch (block.type) {
      case 'code': {
        const pre = element('pre', 'md-pre');
        const code = element('code', 'md-code-block', [block.text]);
        code.setAttribute('data-lang', (block.lang || '').split(/\s+/)[0] || 'text');
        code.setAttribute('data-block-index', String(codeIndex));
        if (!block.closed) code.setAttribute('data-closed', '0');
        pre.append(code);
        fragment.append(element('div', 'md-code-wrap', [pre]));
        codeIndex += 1;
        break;
      }
      case 'heading': {
        const level = Math.min(Math.max(block.level, 1), 6);
        fragment.append(element('h' + level, 'md-heading', renderInline(block.text)));
        break;
      }
      case 'hr':
        fragment.append(element('hr', 'md-hr'));
        break;
      case 'quote':
        fragment.append(element('blockquote', 'md-quote', renderInline(block.lines.join('\n'))));
        break;
      case 'table': {
        const head = element('tr', null, block.header.map((cell) => element('th', null, renderInline(cell))));
        const body = block.rows.map((row) => element('tr', null,
          block.header.map((_, index) => element('td', null, renderInline(row[index] || '')))));
        fragment.append(element('div', 'md-table-wrap', [
          element('table', 'md-table', [element('thead', null, [head]), element('tbody', null, body)]),
        ]));
        break;
      }
      case 'list': {
        const list = element(block.ordered ? 'ol' : 'ul', 'md-list',
          block.items.map((item) => element('li', null, renderInline(item))));
        fragment.append(list);
        break;
      }
      default:
        fragment.append(element('p', 'md-paragraph', renderInline(block.text)));
    }
  }
  return fragment;
}
