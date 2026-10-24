// Highlights in the editor (SPEC.md §19). The text holds =={name}text==; the editor shows only the
// coloured text. Delimiters are hidden (and atomic) unless the selection touches the highlight, when
// they show dimmed so it is always visible whether typing lands inside or outside it.
import {
  Annotation,
  ChangeSet,
  EditorSelection,
  EditorState,
  RangeSet,
  RangeValue,
  StateEffect,
  StateField,
  Transaction,
  type ChangeSpec,
  type Extension,
  type Line,
  type Range,
  type Text,
  type TransactionSpec,
} from '@codemirror/state';
import { Decoration, EditorView, ViewPlugin, type DecorationSet, type ViewUpdate } from '@codemirror/view';
import { isolateHistory } from '@codemirror/commands';
import { hlDeep, hlHue, lineRuns, parseLine, parseStyle, restyle, serialize, stripHighlights, writeLine, type Hl, type Style } from '../highlight';
import { MARKER_RE } from '../ids';
import { headingSlice, isHeadingLine, stripSections } from '../heading';
import { imageOp, isImageLine } from './images';

/** Our own highlight edits: the guard and the selection normaliser leave them alone. */
export const highlightOp = Annotation.define<boolean>();

// ---- the index: every highlight in the document ----

class HlMark extends RangeValue {
  constructor(
    readonly name: string,
    readonly open: number,
  ) {
    super();
  }
  eq(o: HlMark) {
    return o.name === this.name && o.open === this.open;
  }
}

const toHl = (from: number, to: number, v: HlMark): Hl => ({ from, to, innerFrom: from + v.open, innerTo: to - 2, name: v.name });

function scanLines(doc: Text, from: number, to: number): Range<HlMark>[] {
  const out: Range<HlMark>[] = [];
  for (let pos = from; pos <= to; ) {
    const line = doc.lineAt(pos);
    for (const h of parseLine(line.text, line.from)) out.push(new HlMark(h.name, h.innerFrom - h.from).range(h.from, h.to));
    pos = line.to + 1;
  }
  return out;
}

const hlField = StateField.define<RangeSet<HlMark>>({
  create: (s) => RangeSet.of(scanLines(s.doc, 0, s.doc.length), true),
  update(set, tr) {
    if (!tr.docChanged) return set;
    set = set.map(tr.changes);
    // rescan every changed line (a mapped highlight may no longer parse the same way)
    const doc = tr.newDoc;
    const spans: [number, number][] = [];
    tr.changes.iterChangedRanges((_fa, _ta, fromB, toB) => {
      const a = doc.lineAt(fromB).from;
      const b = doc.lineAt(toB).to;
      const last = spans[spans.length - 1];
      if (last && a <= last[1] + 1) last[1] = Math.max(last[1], b);
      else spans.push([a, b]);
    });
    for (const [a, b] of spans) {
      set = set.update({ filterFrom: a, filterTo: b, filter: (f, t) => t < a || f > b });
      const add = scanLines(doc, a, b);
      if (add.length) set = set.update({ add, sort: true });
    }
    return set;
  },
});

/** All highlights overlapping or touching [from, to]. */
export function highlightsIn(state: EditorState, from: number, to: number): Hl[] {
  const out: Hl[] = [];
  state.field(hlField).between(from, to, (f, t, v) => void out.push(toHl(f, t, v)));
  return out;
}

/** The highlight the selection is in. A bare cursor must be in its text (inner edges count), so a
 * cursor just outside a highlight never targets it. */
export function highlightAt(state: EditorState): Hl | null {
  const r = state.selection.main;
  const hs = highlightsIn(state, r.from, r.to);
  return (r.empty ? hs.find((h) => h.innerFrom <= r.head && r.head <= h.innerTo) : hs.find((h) => h.from <= r.from && r.to <= h.to)) ?? null;
}

// ---- drawing: coloured text, hidden or revealed delimiters ----

const refresh = StateEffect.define<null>();
const hidden = Decoration.replace({});
const delim = Decoration.mark({ class: 'hl-delim' });
const markCache = new Map<string, Decoration>();
const markFor = (name: string) => {
  let d = markCache.get(name);
  if (!d) {
    // the tint only with a colour; bold and underline on their own
    const st = parseStyle(name);
    const cls = [st.color ? (hlDeep(st.color) ? 'hl hl-deep' : 'hl') : '', st.b ? 'fmt-b' : '', st.u ? 'fmt-u' : ''].filter(Boolean).join(' ');
    const attributes: Record<string, string> = { 'data-hl': name };
    if (st.color) attributes.style = `--h:${hlHue(st.color)}`;
    d = Decoration.mark({ class: cls, attributes });
    markCache.set(name, d);
  }
  return d;
};

const drawer = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet = Decoration.none;
    atomic: RangeSet<Decoration> = RangeSet.empty;
    revealed = new Set<number>();
    pointer = false;

    constructor(view: EditorView) {
      this.compute(view, true);
    }

    update(u: ViewUpdate) {
      const poked = u.transactions.some((t) => t.effects.some((e) => e.is(refresh)));
      // while the mouse button is down nothing is revealed or hidden, so text never moves under the pointer
      const reveal = poked || u.docChanged || (u.selectionSet && !this.pointer);
      if (reveal || u.viewportChanged) this.compute(u.view, reveal);
    }

    compute(view: EditorView, reveal: boolean) {
      const { state } = view;
      const set = state.field(hlField);
      if (reveal) {
        this.revealed = new Set();
        for (const r of state.selection.ranges) set.between(r.from, r.to, (f) => void this.revealed.add(f));
      }
      const atomic: Range<Decoration>[] = [];
      for (let it = set.iter(); it.value; it.next()) {
        if (this.revealed.has(it.from)) continue;
        atomic.push(hidden.range(it.from, it.from + it.value.open), hidden.range(it.to - 2, it.to));
      }
      this.atomic = RangeSet.of(atomic);
      const decos: Range<Decoration>[] = [];
      for (const { from, to } of view.visibleRanges) {
        set.between(from, to, (f, t, v) => {
          const show = this.revealed.has(f);
          decos.push((show ? delim : hidden).range(f, f + v.open));
          decos.push(markFor(v.name).range(f + v.open, t - 2));
          decos.push((show ? delim : hidden).range(t - 2, t));
        });
      }
      this.decorations = Decoration.set(decos, true);
    }
  },
  {
    decorations: (p) => p.decorations,
    provide: (plugin) =>
      EditorView.atomicRanges.of((view) => view.plugin(plugin)?.atomic ?? RangeSet.empty),
    eventHandlers: {
      mousedown(e, view) {
        if (e.button !== 0) return false;
        this.pointer = true;
        const up = () => {
          window.removeEventListener('mouseup', up, true);
          this.pointer = false;
          view.dispatch({ effects: refresh.of(null) });
        };
        window.addEventListener('mouseup', up, true);
        return false;
      },
    },
  },
);

// ---- the cursor never rests inside a delimiter ----

function outOfDelims(doc: Text, pos: number, forward: boolean): number {
  const line = doc.lineAt(pos);
  for (const h of parseLine(line.text, line.from)) {
    if (pos > h.from && pos < h.innerFrom) return forward ? h.innerFrom : h.from;
    if (pos > h.innerTo && pos < h.to) return forward ? h.to : h.innerTo;
  }
  return pos;
}

const normalize = EditorState.transactionFilter.of((tr) => {
  if ((!tr.selection && !tr.docChanged) || tr.annotation(highlightOp) || tr.isUserEvent('select.search')) return tr;
  const sel = tr.newSelection;
  const old = tr.startState.selection.main.head;
  let changed = false;
  const ranges = sel.ranges.map((r) => {
    const fwd = r.head >= old;
    const head = outOfDelims(tr.newDoc, r.head, fwd);
    const anchor = r.empty ? head : outOfDelims(tr.newDoc, r.anchor, !fwd);
    if (head === r.head && anchor === r.anchor) return r;
    changed = true;
    return EditorSelection.range(anchor, head);
  });
  return changed ? [tr, { selection: EditorSelection.create(ranges, sel.mainIndex), sequential: true }] : tr;
});

// ---- edits keep highlights well-formed ----

interface Change {
  from: number;
  to: number;
  insert: string;
}

/** The ranges of [from, to) left after cutting out `holes`. */
function subtract(from: number, to: number, holes: [number, number][]): [number, number][] {
  let parts: [number, number][] = [[from, to]];
  for (const [a, b] of holes) {
    parts = parts.flatMap(([f, t]): [number, number][] => {
      if (b <= f || a >= t) return [[f, t]];
      const out: [number, number][] = [];
      if (f < a) out.push([f, a]);
      if (b < t) out.push([b, t]);
      return out;
    });
  }
  return parts;
}

/**
 * User edits that touch a highlight are reshaped so the text stays valid:
 * - delimiters are never partly edited: an edit crossing one skips it, and Backspace/Delete on a
 *   (revealed) delimiter removes the whole highlight, keeping its text;
 * - a highlight whose text changed is written again if it no longer parses as the same highlight:
 *   a line break inside it (Enter, paste) closes and reopens it on each line, "==" typed inside
 *   splits it, and one left with no text loses its delimiters.
 */
const guard = EditorState.transactionFilter.of((tr): TransactionSpec | readonly TransactionSpec[] => {
  if (!tr.docChanged || tr.annotation(highlightOp) || tr.annotation(imageOp) || !tr.annotation(Transaction.userEvent)) return tr;
  const start = tr.startState;
  const doc = start.doc;
  const orig: Change[] = [];
  tr.changes.iterChanges((from, to, _fb, _tb, ins) => void orig.push({ from, to, insert: ins.toString() }));

  // highlights an edit reaches into (an insertion exactly at either outer edge is outside)
  const touched = new Map<number, Hl>();
  for (const c of orig) {
    for (const h of highlightsIn(start, c.from, c.to)) {
      const inside = c.from === c.to ? c.from > h.from && c.from < h.to : c.from < h.to && c.to > h.from;
      if (inside) touched.set(h.from, h);
    }
  }
  if (!touched.size) return tr;

  // phase A: keep edits off delimiters
  const changes: Change[] = [];
  const unwrap = new Set<Hl>();
  const gone = new Set<Hl>(); // wholly replaced
  // where each original edit's inserted text now starts (the cursor goes after it)
  const lands: { from: number; to: number; at: number; len: number }[] = [];
  let reshaped = false;
  for (const c of orig) {
    const hs = [...touched.values()].filter((h) => (c.from === c.to ? c.from > h.from && c.from < h.to : c.from < h.to && c.to > h.from));
    if (!hs.length) {
      changes.push(c);
      lands.push({ from: c.from, to: c.to, at: c.from, len: c.insert.length });
      continue;
    }
    if (c.from === c.to) {
      // an insertion inside a delimiter goes to the delimiter's inner edge
      let p = c.from;
      for (const h of hs) {
        if (p < h.innerFrom) p = h.innerFrom;
        else if (p > h.innerTo) p = h.innerTo;
        // "=" typed at the end of the text would merge into the closer: it goes after it instead
        if (p === h.innerTo && c.insert.startsWith('=')) p = h.to;
      }
      reshaped ||= p !== c.from;
      changes.push({ ...c, from: p, to: p });
      lands.push({ from: c.from, to: c.to, at: p, len: c.insert.length });
      continue;
    }
    const holes: [number, number][] = [];
    for (const h of hs) {
      if (c.from <= h.from && c.to >= h.to) gone.add(h);
      else holes.push([h.from, h.innerFrom], [h.innerTo, h.to]);
    }
    const parts = subtract(c.from, c.to, holes);
    if (!parts.length) {
      if (c.insert) return {}; // typing over part of a delimiter: refuse
      for (const h of hs) unwrap.add(h); // Backspace/Delete on a delimiter: drop the highlight, keep its text
      reshaped = true;
      continue;
    }
    if (parts.length !== 1 || parts[0][0] !== c.from || parts[0][1] !== c.to) reshaped = true;
    parts.forEach(([f, t], i) => changes.push({ from: f, to: t, insert: i === 0 ? c.insert : '' }));
    lands.push({ from: c.from, to: c.to, at: parts[0][0], len: c.insert.length });
  }
  for (const h of unwrap) changes.push({ from: h.from, to: h.innerFrom, insert: '' }, { from: h.innerTo, to: h.to, insert: '' });
  changes.sort((a, b) => a.from - b.from || a.to - b.to);
  for (let i = 1; i < changes.length; i++) if (changes[i].from < changes[i - 1].to) return {}; // overlapping (multi-cursor): refuse

  const csA = reshaped ? ChangeSet.of(changes, doc.length) : tr.changes;
  const docA = reshaped ? csA.apply(doc) : tr.newDoc;
  // after a reshaped edit the cursor goes right after what was typed (or where text was deleted)
  const cursorFor = (r: { from: number; to: number; head: number }) => {
    const l = lands.find((l) => l.from <= r.head && r.head <= l.to && l.from <= r.from && r.to <= l.to) ?? lands.find((l) => l.from <= r.head && r.head <= l.to);
    return l ? csA.mapPos(l.at, -1) + l.len : csA.mapPos(r.head, 1);
  };
  const selA = reshaped ? EditorSelection.create(start.selection.ranges.map((r) => EditorSelection.cursor(cursorFor(r))), start.selection.mainIndex) : tr.newSelection;

  // phase B: rewrite highlights whose text no longer parses as the same highlight
  const fixes: { from: number; to: number; text: string; same: boolean; map: (pos: number) => number }[] = [];
  for (const h of touched.values()) {
    if (unwrap.has(h) || gone.has(h)) continue;
    const from = csA.mapPos(h.from, 1);
    const to = csA.mapPos(h.to, -1);
    const open = h.innerFrom - h.from;
    if (to - from < open + 2) continue;
    const line = docA.lineAt(from);
    const inner = docA.sliceString(from + open, to - 2);
    const intact = to <= line.to && parseLine(line.text, line.from).some((x) => x.from === from && x.to === to && x.name === h.name);
    if (intact && /\S/.test(inner)) continue; // still the same highlight, with something to show
    const segs = inner.split('\n');
    let text = '';
    const starts: number[] = []; // where each line's text starts in the rewritten text
    const outs: ReturnType<typeof serialize>[] = [];
    segs.forEach((seg, i) => {
      if (i) text += '\n';
      starts.push(text.length);
      const plainSeg = serialize(seg, Array(seg.length).fill(null));
      const out = MARKER_RE.test(seg) || isHeadingLine(seg) ? plainSeg : (writeLine(seg, Array(seg.length).fill(h.name)) ?? plainSeg);
      outs.push(out);
      text += out.text;
    });
    const innerFrom = from + open;
    const map = (pos: number) => {
      let off = Math.max(0, Math.min(inner.length, pos - innerFrom));
      let i = 0;
      while (i < segs.length - 1 && off > segs[i].length) off -= segs[i++].length + 1;
      return from + starts[i] + (off === 0 ? outs[i].after[0] : outs[i].before[off]);
    };
    // unchanged text still maps the cursor (a "=" typed at the end lands after the closer)
    fixes.push({ from, to, text, same: text === docA.sliceString(from, to), map });
  }
  fixes.sort((a, b) => a.from - b.from);
  if (!reshaped && !fixes.length) return tr;

  const annotations: Annotation<unknown>[] = [Transaction.userEvent.of(tr.annotation(Transaction.userEvent)!)];
  const toHistory = tr.annotation(Transaction.addToHistory);
  if (toHistory !== undefined) annotations.push(Transaction.addToHistory.of(toHistory));
  const isolate = tr.annotation(isolateHistory);
  if (isolate) annotations.push(isolateHistory.of(isolate));
  const first: TransactionSpec = reshaped ? { changes: csA, selection: selA, annotations, scrollIntoView: tr.scrollIntoView, effects: tr.effects } : tr;
  if (!fixes.length) return first;
  const mapB = (pos: number) => {
    let delta = 0;
    for (const f of fixes) {
      if (pos <= f.from) break; // at the outer edges a position stays outside
      if (pos < f.to) return f.map(pos) + delta;
      delta += f.text.length - (f.to - f.from);
    }
    return pos + delta;
  };
  const sel = EditorSelection.create(selA.ranges.map((r) => EditorSelection.range(mapB(r.anchor), mapB(r.head))), selA.mainIndex);
  const edits = fixes.filter((f) => !f.same).map(({ from, to, text }) => ({ from, to, insert: text }));
  return [first, { changes: edits, selection: sel, sequential: true }];
});

/** Search-and-replace never edits a delimiter (replacing "coral" can't recolour a highlight). */
const protectOnReplace = EditorState.changeFilter.of((tr) => {
  if (!tr.isUserEvent('input.replace')) return true;
  const out: number[] = [];
  for (let it = tr.startState.field(hlField).iter(); it.value; it.next()) out.push(it.from, it.from + it.value.open, it.to - 2, it.to);
  return out.length ? out : true;
});

// ---- clipboard: other apps get clean text, this app keeps the highlights ----

export const RAW_MIME = 'text/x-notes-raw';
let lastCopy: { plain: string; raw: string } | null = null;
let pasting: { plain: string; raw: string } | null = null;

/** [from, to) as plain text, and as raw text with balanced delimiters (a highlight cut by the
 * selection is closed/opened at the cut). Works by position, so no half delimiter ever leaks. */
export function cleanSlice(state: EditorState, from: number, to: number): { plain: string; raw: string } {
  const doc = state.doc;
  let plain = '';
  let raw = '';
  for (let pos = from; ; ) {
    const line = doc.lineAt(pos);
    const a = Math.max(from, line.from) - line.from;
    const b = Math.min(to, line.to) - line.from;
    const head = headingSlice(line.text, a, b); // a section heading: other apps get just its name
    if (head) {
      plain += head.plain;
      raw += head.raw;
    } else {
      const runs = lineRuns(line.text);
      const ps = runs.toPlain(a);
      const pe = runs.toPlain(b);
      const p = runs.plain.slice(ps, pe);
      plain += p;
      raw += serialize(p, runs.colors.slice(ps, pe)).text;
    }
    if (line.to >= to) break;
    plain += '\n';
    raw += '\n';
    pos = line.to + 1;
  }
  return { plain, raw };
}

// CodeMirror builds the copied text from the selection (or whole lines for a bare cursor; the main
// range when dragging); the same ranges are rebuilt here from positions.
const copyOut = EditorView.clipboardOutputFilter.of((text, state) => {
  const br = state.lineBreak;
  const ranges = state.selection.ranges.filter((r) => !r.empty);
  const lines = [...new Set(state.selection.ranges.map((r) => state.doc.lineAt(r.head).number))].map((n) => state.doc.line(n));
  const main = state.selection.main;
  let parts: { plain: string; raw: string }[] | null = null;
  if (ranges.length && ranges.map((r) => state.sliceDoc(r.from, r.to)).join(br) === text) parts = ranges.map((r) => cleanSlice(state, r.from, r.to));
  else if (!ranges.length && lines.map((l) => l.text).join(br) === text) parts = lines.map((l) => cleanSlice(state, l.from, l.to));
  else if (text === state.sliceDoc(main.from, main.to)) parts = [cleanSlice(state, main.from, main.to)];
  if (!parts) {
    lastCopy = null;
    return stripSections(stripHighlights(text));
  }
  const plain = parts.map((p) => p.plain).join(br);
  const raw = parts.map((p) => p.raw).join(br);
  lastCopy = plain === raw ? null : { plain, raw };
  return plain;
});

const pasteIn = EditorView.clipboardInputFilter.of((text, state) => {
  const p = pasting;
  pasting = null;
  if (!p) return text;
  // other filters may have put the text on its own line(s) (image lines): keep that padding
  const at = text.indexOf(p.plain);
  if (at < 0 || !/^\n*$/.test(text.slice(0, at)) || !/^\n*$/.test(text.slice(at + p.plain.length))) return text;
  // pasted into a highlight, the text takes that highlight's colour instead
  const r = state.selection.main;
  if (highlightsIn(state, r.from, r.to).some((h) => r.from >= h.innerFrom && r.to <= h.innerTo)) return text;
  // a copied section heading only becomes one again at the start of a line; mid-line it pastes as its name
  if (at === 0 && isHeadingLine(p.raw.split('\n')[0]) && state.doc.lineAt(r.from).from !== r.from) return text;
  return text.slice(0, at) + p.raw + text.slice(at + p.plain.length);
});

const transferEvents = EditorView.domEventHandlers({
  paste(e) {
    const raw = e.clipboardData?.getData(RAW_MIME);
    pasting = raw ? { plain: e.clipboardData!.getData('text/plain'), raw } : null;
    return false;
  },
  drop(e) {
    const raw = e.dataTransfer?.getData(RAW_MIME);
    pasting = raw ? { plain: e.dataTransfer!.getData('Text'), raw } : null;
    return false;
  },
});

// CodeMirror writes the plain text in its own copy/cut/dragstart handlers; ours run after them and add
// the raw text.
const rawOnCopy = ViewPlugin.define((view) => {
  const add = (e: ClipboardEvent | DragEvent) => {
    const data = 'clipboardData' in e ? e.clipboardData : e.dataTransfer;
    if (lastCopy && data && data.getData('text/plain') === lastCopy.plain) data.setData(RAW_MIME, lastCopy.raw);
  };
  const types = ['copy', 'cut', 'dragstart'] as const;
  for (const t of types) view.contentDOM.addEventListener(t, add as EventListener);
  return {
    destroy() {
      for (const t of types) view.contentDOM.removeEventListener(t, add as EventListener);
    },
  };
});

/** What the highlight UI needs to know about the selection. rect: the selection start, in viewport coordinates. */
export interface HighlightInfo {
  can: boolean; // a highlight can be applied/changed here
  empty: boolean; // just a cursor (inside a highlight)
  current: string | null; // the colour, if the whole selection has one and the same colour
  focused: boolean;
  dragging: boolean;
  rect: { left: number; top: number; bottom: number } | null;
  bold: Coverage;
  underline: Coverage;
  hasColor: boolean; // some selected character has a colour
  styled: boolean; // some selected character has a colour, bold or underline
}

export type Coverage = 'all' | 'some' | 'none';

// ---- the selection's styles ----

/** The ranges a style edit touches: per text line (images and section headings skipped), as plain
 * offsets. With just a cursor, the whole text of the highlight it is in (untrimmed). */
function targets(state: EditorState): { line: Line; runs: ReturnType<typeof lineRuns>; ps: number; pe: number; cursor: boolean }[] {
  let { from, to } = state.selection.main;
  let cursor = false;
  if (from === to) {
    const h = highlightAt(state);
    if (!h) return [];
    from = h.innerFrom;
    to = h.innerTo;
    cursor = true;
  }
  const out = [];
  for (let pos = from; pos <= to; ) {
    const line = state.doc.lineAt(pos);
    const a = Math.max(from, line.from) - line.from;
    const b = Math.min(to, line.to) - line.from;
    pos = line.to + 1;
    if (isImageLine(line) || isHeadingLine(line.text) || a >= b) continue;
    const runs = lineRuns(line.text);
    out.push({ line, runs, ps: runs.toPlain(a), pe: runs.toPlain(b), cursor });
  }
  return out;
}

/** Bold / underline / colour over the selection's visible (non-whitespace) characters. */
export function styleState(state: EditorState): { bold: Coverage; underline: Coverage; hasColor: boolean; styled: boolean; color: string | null; any: boolean } {
  let n = 0;
  let nb = 0;
  let nu = 0;
  let colored = 0;
  let styled = 0;
  const colors = new Set<string | null>();
  for (const t of targets(state)) {
    for (let i = t.ps; i < t.pe; i++) {
      if (/\s/.test(t.runs.plain[i])) continue;
      const st = parseStyle(t.runs.colors[i]);
      n++;
      if (st.b) nb++;
      if (st.u) nu++;
      if (st.color) colored++;
      if (st.color || st.b || st.u) styled++;
      colors.add(st.color);
    }
  }
  const cov = (k: number): Coverage => (n && k === n ? 'all' : k ? 'some' : 'none');
  const color = colors.size === 1 ? ([...colors][0] ?? null) : null;
  return { bold: cov(nb), underline: cov(nu), hasColor: colored > 0, styled: styled > 0, color: colored === n ? color : null, any: n > 0 };
}

export type StyleOp = { kind: 'color'; name: string | null } | { kind: 'toggle'; style: 'b' | 'u' } | { kind: 'clear' };

/**
 * Change the selection's styles (SPEC §19): set or remove the colour (bold and underline stay),
 * toggle bold or underline (decided once for the whole selection: all have it → remove, else add),
 * or clear everything. Each text line is handled on its own; images and section headings are
 * skipped. With only a cursor, the highlight it is in. One undo step; false if nothing to do.
 */
export function applyStyle(view: Pick<EditorView, 'state' | 'dispatch'>, op: StyleOp): boolean {
  const { state } = view;
  if (state.readOnly) return false;
  const ts = targets(state);
  if (!ts.length) return false;
  let fn: (s: Style) => Style;
  let adding: boolean;
  if (op.kind === 'color') {
    fn = (s) => ({ ...s, color: op.name });
    adding = op.name !== null;
  } else if (op.kind === 'toggle') {
    const st = styleState(state);
    if (!st.any) return false;
    const add = (op.style === 'b' ? st.bold : st.underline) !== 'all';
    fn = (s) => ({ ...s, [op.style]: add });
    adding = add;
  } else {
    fn = () => ({ color: null, b: false, u: false });
    adding = false;
  }
  const changes: ChangeSpec[] = [];
  let selFrom: number | null = null;
  let selTo = 0;
  let delta = 0;
  for (const t of ts) {
    const res = restyle(t.runs, t.ps, t.pe, fn, adding && !t.cursor);
    if (!res) continue;
    if (res.text !== t.line.text) changes.push({ from: t.line.from, to: t.line.to, insert: res.text });
    const base = t.line.from + delta;
    selFrom ??= base + res.after[res.ps];
    selTo = base + res.before[res.pe];
    delta += res.text.length - t.line.text.length;
  }
  if (selFrom === null) return false;
  if (!changes.length && state.selection.main.empty) return true;
  view.dispatch({
    changes,
    selection: state.selection.main.empty ? undefined : EditorSelection.single(selFrom, selTo),
    annotations: [highlightOp.of(true), isolateHistory.of('full')],
    userEvent: 'input.highlight',
    scrollIntoView: true,
  });
  return true;
}

// ---- applying and removing ----

/**
 * Colour the main selection with `name` (null removes the colour; bold and underline stay).
 * Kept for callers that only deal with colours; see applyStyle.
 */
export function applyHighlight(view: Pick<EditorView, 'state' | 'dispatch'>, name: string | null): boolean {
  return applyStyle(view, { kind: 'color', name });
}

export function highlights(): Extension {
  return [hlField, drawer, normalize, guard, protectOnReplace, copyOut, pasteIn, transferEvents, rawOnCopy];
}
