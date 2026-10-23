// Text highlights stored in the note itself: =={name}text== (SPEC.md §19). Pure text helpers,
// shared by the editor, the footer counts and the tests. The server has the same grammar in
// internal/core/highlight.go; both are checked against internal/core/testdata/highlights.json.
import { HUES } from './colors';
import { MARKER_RE } from './ids';

/** 12 hues (the tag palette), each light and "-deep": 24 colours. */
export const HL_HUES = Object.keys(HUES);
export const HL_NAMES: string[] = HL_HUES.flatMap((h) => [h, `${h}-deep`]);
const NAME_SET = new Set(HL_NAMES);

export const hlHue = (name: string) => HUES[name.replace(/-deep$/, '')] ?? 262;
export const hlDeep = (name: string) => name.endsWith('-deep');
export const hlLabel = (name: string) => {
  const base = name.replace(/-deep$/, '');
  return base[0].toUpperCase() + base.slice(1) + (hlDeep(name) ? ', strong' : '');
};
export const isHlName = (s: string) => NAME_SET.has(s);

// Grammar: ==\{(name)\}([^\n]+?)== — a known name, at least one character, ending at the first "=="
// after it, never across lines. Parsed by hand in linear time (a regex with a lazy body rescans the
// rest of the line for every unclosed opener); the Go side uses an RE2 regexp, which is linear too.

/** A highlight: [from, to) is the whole thing, [innerFrom, innerTo) the coloured text. */
export interface Hl {
  from: number;
  to: number;
  innerFrom: number;
  innerTo: number;
  name: string;
}

const isMarkerLine = (text: string) => text.startsWith('[image:') && MARKER_RE.test(text);

/** Highlights on one line of text; offsets are relative to `base`. Image lines have none. */
export function parseLine(text: string, base = 0): Hl[] {
  if (!text.includes('=={') || isMarkerLine(text)) return [];
  // nextEq[i]: first j >= i where "==" starts
  const n = text.length;
  const nextEq = new Int32Array(n + 1).fill(-1);
  for (let i = n - 2; i >= 0; i--) nextEq[i] = text[i] === '=' && text[i + 1] === '=' ? i : nextEq[i + 1];
  const out: Hl[] = [];
  for (let i = text.indexOf('=={'); i >= 0 && i < n; ) {
    const close = text.indexOf('}', i + 3);
    const name = close < 0 ? '' : text.slice(i + 3, close);
    const end = close >= 0 && NAME_SET.has(name) && close + 2 <= n ? nextEq[close + 2] : -1;
    if (end < 0) {
      i = text.indexOf('=={', i + 1);
      continue;
    }
    out.push({ from: base + i, to: base + end + 2, innerFrom: base + close + 1, innerTo: base + end, name });
    i = text.indexOf('=={', end + 2);
  }
  return out;
}

/** Where a line's text visually starts and ends: inside a highlight that opens or closes the line,
 * the hidden delimiter sits between the line edge and the first/last visible character. */
export function visualEdges(text: string): { start: number; end: number } {
  if (!text.includes('=={')) return { start: 0, end: text.length };
  const hs = parseLine(text);
  const first = hs[0];
  const last = hs[hs.length - 1];
  return { start: first?.from === 0 ? first.innerFrom : 0, end: last?.to === text.length ? last.innerTo : text.length };
}

/** The text without highlight delimiters (what titles, counts and search see). */
export function stripHighlights(text: string): string {
  if (!text.includes('=={')) return text;
  return text
    .split('\n')
    .map((l) => {
      const hs = parseLine(l);
      if (!hs.length) return l;
      let out = '';
      let at = 0;
      for (const h of hs) {
        out += l.slice(at, h.from) + l.slice(h.innerFrom, h.innerTo);
        at = h.to;
      }
      return out + l.slice(at);
    })
    .join('\n');
}

/** A line as plain text plus a colour per character. */
export interface Runs {
  plain: string;
  colors: (string | null)[];
  /** raw offset (0..text.length) → plain offset; offsets inside a delimiter go to its inner edge */
  toPlain: (raw: number) => number;
}

export function lineRuns(text: string): Runs {
  const hs = parseLine(text);
  let plain = '';
  const colors: (string | null)[] = [];
  const map = new Array<number>(text.length + 1);
  let at = 0;
  const copy = (from: number, to: number, color: string | null) => {
    for (let i = from; i < to; i++) {
      map[i] = plain.length;
      plain += text[i];
      colors.push(color);
    }
  };
  for (const h of hs) {
    copy(at, h.from, null);
    for (let i = h.from; i < h.innerFrom; i++) map[i] = plain.length; // opener → start of the text
    copy(h.innerFrom, h.innerTo, h.name);
    for (let i = h.innerTo; i < h.to; i++) map[i] = plain.length; // closer → end of the text
    at = h.to;
  }
  copy(at, text.length, null);
  map[text.length] = plain.length;
  return { plain, colors, toPlain: (raw) => map[Math.max(0, Math.min(text.length, raw))] };
}

/** The coloured stretches that can actually be written: a stretch never contains "==" (it would
 * end the highlight early), never ends with "=" (the closer would swallow it), and is never only
 * whitespace. Those characters simply stay uncoloured. */
export function pieces(plain: string, colors: (string | null)[]): { s: number; e: number; name: string }[] {
  const out: { s: number; e: number; name: string }[] = [];
  const push = (s: number, e: number, name: string) => {
    while (e > s && plain[e - 1] === '=') e--;
    if (e > s && /\S/.test(plain.slice(s, e))) out.push({ s, e, name });
  };
  let i = 0;
  while (i < plain.length) {
    const name = colors[i];
    if (!name) {
      i++;
      continue;
    }
    let s = i;
    while (i < plain.length && colors[i] === name) {
      if (plain[i] === '=' && plain[i + 1] === '=' && colors[i + 1] === name) {
        push(s, i, name);
        while (i < plain.length && plain[i] === '=' && colors[i] === name) i++;
        s = i;
        continue;
      }
      i++;
    }
    push(s, i, name);
  }
  return out;
}

/** Write a line back from plain text + colours. before[i]/after[i] give the raw position for plain
 * offset i, attached to the character before it (inside a closing highlight) or after it (inside an
 * opening one). */
export function serialize(plain: string, colors: (string | null)[]): { text: string; before: number[]; after: number[] } {
  const ps = pieces(plain, colors);
  const before: number[] = [];
  const after: number[] = [];
  let out = '';
  let next = 0;
  let open: { e: number } | null = null;
  for (let i = 0; i <= plain.length; i++) {
    before[i] = out.length;
    if (open && open.e === i) {
      out += '==';
      open = null;
    }
    if (next < ps.length && ps[next].s === i) {
      out += `=={${ps[next].name}}`;
      open = ps[next++];
    }
    after[i] = out.length;
    if (i < plain.length) out += plain[i];
  }
  return { text: out, before, after };
}

/** One line with plain offsets [ps, pe) set to `name` (null removes colour). With trim, the
 * range shrinks to its non-whitespace extent first. Returns null if the range is empty after trim. */
export function recolor(runs: Runs, ps: number, pe: number, name: string | null, trim = true) {
  const { plain } = runs;
  if (trim && name) {
    while (ps < pe && /\s/.test(plain[ps])) ps++;
    while (pe > ps && /\s/.test(plain[pe - 1])) pe--;
  }
  // never split a surrogate pair (an emoji)
  if (ps > 0 && /[\udc00-\udfff]/.test(plain[ps] ?? '')) ps--;
  if (pe < plain.length && /[\ud800-\udbff]/.test(plain[pe - 1] ?? '')) pe++;
  if (ps >= pe) return null;
  const colors = runs.colors.slice();
  for (let i = ps; i < pe; i++) colors[i] = name;
  const out = writeLine(plain, colors);
  return out && { ...out, ps, pe };
}

/** serialize(), but only if the result reads back as exactly that text and those colours. Plain text
 * next to a new delimiter can change how a line parses (a stray, unclosed "=={coral}" would be
 * completed by the next "=="); then null, and the caller leaves the line alone. */
export function writeLine(plain: string, colors: (string | null)[]) {
  const out = serialize(plain, colors);
  const back = lineRuns(out.text);
  if (back.plain !== plain) return null;
  const want = pieces(plain, colors);
  const got = pieces(back.plain, back.colors);
  if (want.length !== got.length || want.some((p, i) => p.s !== got[i].s || p.e !== got[i].e || p.name !== got[i].name)) return null;
  return out;
}
