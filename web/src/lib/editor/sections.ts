// Sections in the editor (SPEC.md §22). A line "## {colour} Name" starts a section that runs to the next
// heading or the end of the note. The editor shows only the name, as a coloured heading; the body gets a
// bar in the same hue. "## {colour} " is hidden (and atomic) unless the selection touches the heading,
// when it shows dimmed. The title line is never a section.
import {
  Annotation,
  EditorSelection,
  EditorState,
  RangeSet,
  RangeValue,
  StateEffect,
  StateField,
  Transaction,
  type Extension,
  type Range,
  type Text,
  type TransactionSpec,
} from '@codemirror/state';
import { Decoration, EditorView, RectangleMarker, ViewPlugin, WidgetType, layer, type DecorationSet, type ViewUpdate } from '@codemirror/view';
import { codeFolding, foldEffect, foldedRanges, unfoldEffect } from '@codemirror/language';
import { isolateHistory } from '@codemirror/commands';
import { hlHue, stripHighlights } from '../highlight';
import { headingLine, parseHeading } from '../heading';
import { MARKER_RE } from '../ids';
import { imageOp, selectedImage } from './images';
import { highlightOp } from './highlights';

/** Our own section edits: the guard and the normaliser leave them alone. */
export const sectionOp = Annotation.define<boolean>();

/** What the app shows about a section (list, menus). */
export interface SectionInfo {
  name: string;
  color: string;
  folded: boolean;
  foldable: boolean;
}

export interface Section {
  index: number;
  /** the heading line */
  from: number;
  headTo: number;
  prefix: number;
  color: string;
  name: string;
  /** end of the section's last line (the line before the next heading, or the end of the note) */
  end: number;
  /** end of its last non-blank line (headTo if the body is empty) */
  bodyEnd: number;
}

// ---- the index: every heading-shaped line ----

class HeadMark extends RangeValue {
  constructor(readonly prefix: number) {
    super();
  }
  eq(o: HeadMark) {
    return o.prefix === this.prefix;
  }
}

function scanLines(doc: Text, from: number, to: number): Range<HeadMark>[] {
  const out: Range<HeadMark>[] = [];
  for (let pos = from; pos <= to; ) {
    const line = doc.lineAt(pos);
    const h = line.text.startsWith('## {') ? parseHeading(line.text) : null;
    if (h) out.push(new HeadMark(h.prefix).range(line.from, line.to));
    pos = line.to + 1;
  }
  return out;
}

const headField = StateField.define<RangeSet<HeadMark>>({
  create: (s) => RangeSet.of(scanLines(s.doc, 0, s.doc.length), true),
  update(set, tr) {
    if (!tr.docChanged) return set;
    set = set.map(tr.changes);
    const doc = tr.newDoc;
    tr.changes.iterChangedRanges((_fa, _ta, fromB, toB) => {
      const a = doc.lineAt(fromB).from;
      const b = doc.lineAt(toB).to;
      set = set.update({ filterFrom: a, filterTo: b, filter: (f, t) => t < a || f > b });
      const add = scanLines(doc, a, b);
      if (add.length) set = set.update({ add, sort: true });
    });
    return set;
  },
});

/** Start of the title line (the first line with text that isn't an image), or -1. */
function titleFrom(doc: Text): number {
  for (let i = 1; i <= doc.lines; i++) {
    const l = doc.line(i);
    if (stripHighlights(l.text).trim() !== '' && !MARKER_RE.test(l.text)) return l.from;
  }
  return -1;
}

const cache = new WeakMap<Text, Section[]>();

/** The note's sections, in order. */
export function sectionsOf(state: EditorState): Section[] {
  const doc = state.doc;
  const hit = cache.get(doc);
  if (hit) return hit;
  const title = titleFrom(doc);
  const heads: { from: number; to: number; prefix: number }[] = [];
  for (let it = state.field(headField).iter(); it.value; it.next()) {
    if (it.from !== title) heads.push({ from: it.from, to: it.to, prefix: it.value.prefix });
  }
  const out = heads.map((h, index): Section => {
    const next = heads[index + 1];
    const end = next ? next.from - 1 : doc.length;
    let bodyEnd = h.to;
    for (let pos = end; pos > h.to; ) {
      const l = doc.lineAt(pos);
      if (l.text.trim() !== '') {
        bodyEnd = l.to;
        break;
      }
      pos = l.from - 1;
    }
    const text = doc.sliceString(h.from, h.to);
    const parsed = parseHeading(text)!;
    return { index, from: h.from, headTo: h.to, prefix: h.prefix, color: parsed.color, name: parsed.name.trim(), end, bodyEnd };
  });
  cache.set(doc, out);
  return out;
}

/** The section whose heading is on the line at pos. */
export function headingAt(state: EditorState, pos: number): Section | null {
  const line = state.doc.lineAt(pos);
  return sectionsOf(state).find((s) => s.from === line.from) ?? null;
}

/** The section containing pos (its heading or body). */
export function sectionAt(state: EditorState, pos: number): Section | null {
  return sectionsOf(state).find((s) => s.from <= pos && pos <= s.end) ?? null;
}

/** True if the selection touches a heading line (highlights can't go there). */
export function selectionOnHeading(state: EditorState): boolean {
  const r = state.selection.main;
  return sectionsOf(state).some((s) => s.from <= r.to && r.from <= s.headTo);
}

// ---- folding (collapse) ----

const foldRange = (s: Section) => (s.bodyEnd > s.headTo ? { from: s.headTo, to: s.bodyEnd } : null);

export function isFolded(state: EditorState, s: Section): boolean {
  const r = foldRange(s);
  if (!r) return false;
  let found = false;
  foldedRanges(state).between(r.from, r.from, (f) => void (found ||= f === r.from));
  return found;
}

/** Indexes of the collapsed sections. */
export function foldedSections(state: EditorState): number[] {
  return sectionsOf(state)
    .filter((s) => isFolded(state, s))
    .map((s) => s.index);
}

function foldEffects(state: EditorState, s: Section, fold: boolean): StateEffect<unknown>[] {
  const r = foldRange(s);
  if (!r) return [];
  if (fold) return isFolded(state, s) ? [] : [foldEffect.of(r)];
  const out: StateEffect<unknown>[] = [];
  foldedRanges(state).between(r.from, r.from, (f, t) => void (f === r.from && out.push(unfoldEffect.of({ from: f, to: t }))));
  return out;
}

export function setFolded(view: Pick<EditorView, 'state' | 'dispatch'>, index: number, fold: boolean) {
  const s = sectionsOf(view.state)[index];
  if (!s) return;
  if (fold) {
    // a cursor inside the body would reopen it at once: park it on the heading
    const r = view.state.selection.main;
    const inside = r.head > s.headTo && r.head <= s.end;
    view.dispatch({ effects: foldEffects(view.state, s, true), selection: inside ? { anchor: s.headTo } : undefined, annotations: sectionOp.of(true) });
  } else view.dispatch({ effects: foldEffects(view.state, s, false), annotations: sectionOp.of(true) });
}

/** Collapse exactly these sections (restoring a note's saved state). */
export function restoreFolds(view: EditorView, indexes: number[]) {
  const want = new Set(indexes);
  const effects = sectionsOf(view.state).flatMap((s) => foldEffects(view.state, s, want.has(s.index)));
  if (effects.length) view.dispatch({ effects, annotations: sectionOp.of(true) });
}

const folding = codeFolding({
  preparePlaceholder: (state, range) => state.doc.lineAt(range.to).number - state.doc.lineAt(range.from).number,
  placeholderDOM(_view, onclick, lines: number) {
    const b = document.createElement('button');
    b.className = 'sec-folded';
    b.type = 'button';
    b.textContent = `⋯ ${lines} ${lines === 1 ? 'line' : 'lines'}`;
    b.title = 'Show this section';
    b.setAttribute('aria-label', `Show ${lines} hidden ${lines === 1 ? 'line' : 'lines'}`);
    b.onclick = onclick;
    return b;
  },
});

// ---- drawing ----

const refresh = StateEffect.define<null>();
const hidden = Decoration.replace({});
const delim = Decoration.mark({ class: 'sec-delim' });
const headCache = new Map<string, Decoration>();
const headDeco = (color: string, active: boolean) => {
  const key = `${color}|${active}`;
  let d = headCache.get(key);
  if (!d) {
    d = Decoration.line({ class: active ? 'cm-sec-head active' : 'cm-sec-head', attributes: { style: `--h:${hlHue(color)}`, 'data-sec': color } });
    headCache.set(key, d);
  }
  return d;
};

/** Tell the app to open a section's menu (NoteView listens). */
function openMenu(view: EditorView, index: number, anchor: HTMLElement) {
  const r = anchor.getBoundingClientRect();
  view.dom.dispatchEvent(new CustomEvent('notes:section-menu', { bubbles: true, detail: { index, rect: { left: r.left, top: r.top, bottom: r.bottom, right: r.right } } }));
}

class ChevronWidget extends WidgetType {
  constructor(
    readonly index: number,
    readonly folded: boolean,
    readonly foldable: boolean,
  ) {
    super();
  }
  eq(o: ChevronWidget) {
    return o.index === this.index && o.folded === this.folded && o.foldable === this.foldable;
  }
  toDOM(view: EditorView) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'sec-chevron' + (this.folded ? ' folded' : '') + (this.foldable ? '' : ' empty');
    b.tabIndex = -1;
    b.setAttribute('aria-label', this.folded ? 'Show section' : 'Collapse section');
    b.title = this.folded ? 'Show section' : 'Collapse section';
    b.innerHTML = '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><path d="M4 6l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>';
    b.onmousedown = (e) => e.preventDefault();
    b.onclick = (e) => {
      e.preventDefault();
      if (this.foldable) setFolded(view, this.index, !this.folded);
    };
    return b;
  }
  ignoreEvent() {
    return true;
  }
}

class MenuWidget extends WidgetType {
  constructor(readonly index: number) {
    super();
  }
  eq(o: MenuWidget) {
    return o.index === this.index;
  }
  toDOM(view: EditorView) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'sec-menu-btn';
    b.tabIndex = -1;
    b.title = 'Section options';
    b.setAttribute('aria-label', 'Section options');
    b.setAttribute('aria-haspopup', 'menu');
    b.innerHTML = '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><g fill="currentColor"><circle cx="3.5" cy="8" r="1.4"/><circle cx="8" cy="8" r="1.4"/><circle cx="12.5" cy="8" r="1.4"/></g></svg>';
    b.onmousedown = (e) => e.preventDefault();
    b.onclick = (e) => {
      e.preventDefault();
      openMenu(view, this.index, b);
    };
    return b;
  }
  ignoreEvent() {
    return true;
  }
}

const drawer = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet = Decoration.none;
    atomic: RangeSet<Decoration> = RangeSet.empty;
    revealed = new Set<number>();
    pointer = false;
    lastHead = -1; // the heading the cursor was last announced on

    constructor(view: EditorView) {
      this.compute(view, true);
    }

    update(u: ViewUpdate) {
      const poked = u.transactions.some((t) => t.effects.some((e) => e.is(refresh) || e.is(foldEffect) || e.is(unfoldEffect)));
      const reveal = poked || u.docChanged || (u.selectionSet && !this.pointer);
      if (reveal || u.viewportChanged) this.compute(u.view, reveal);
      if (u.selectionSet && !this.pointer) this.announce(u.view);
    }

    /** Moving onto a heading says which section it is (once per heading). */
    announce(view: EditorView) {
      const s = headingAt(view.state, view.state.selection.main.head);
      const at = s?.from ?? -1;
      if (at === this.lastHead) return;
      this.lastHead = at;
      if (s) queueMicrotask(() => view.dispatch({ effects: EditorView.announce.of(`Section, ${s.color.replace('-deep', ' strong')}, ${s.name}`) }));
    }

    compute(view: EditorView, reveal: boolean) {
      const { state } = view;
      const secs = sectionsOf(state);
      if (reveal) {
        this.revealed = new Set();
        for (const r of state.selection.ranges) for (const s of secs) if (s.from <= r.to && r.from <= s.headTo) this.revealed.add(s.from);
      }
      const atomic: Range<Decoration>[] = [];
      const decos: Range<Decoration>[] = [];
      for (const s of secs) {
        const show = this.revealed.has(s.from);
        if (!show) atomic.push(hidden.range(s.from, s.from + s.prefix));
        if (!view.visibleRanges.some((v) => v.from <= s.headTo && s.from <= v.to)) continue;
        const folded = isFolded(state, s);
        decos.push(headDeco(s.color, show).range(s.from));
        decos.push(Decoration.widget({ widget: new ChevronWidget(s.index, folded, s.bodyEnd > s.headTo), side: -1 }).range(s.from));
        decos.push((show ? delim : hidden).range(s.from, s.from + s.prefix));
        decos.push(Decoration.widget({ widget: new MenuWidget(s.index), side: 1 }).range(s.headTo));
      }
      this.atomic = RangeSet.of(atomic, true);
      this.decorations = Decoration.set(decos, true);
    }
  },
  {
    decorations: (p) => p.decorations,
    provide: (plugin) => EditorView.atomicRanges.of((view) => view.plugin(plugin)?.atomic ?? RangeSet.empty),
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

// the bar down each section's body (one continuous bar, past images too), drawn as a layer
const bars = layer({
  above: false,
  class: 'cm-sec-bars',
  update: (u) => u.docChanged || u.geometryChanged || u.viewportChanged || u.transactions.some((t) => t.effects.some((e) => e.is(foldEffect) || e.is(unfoldEffect))),
  markers(view) {
    const out: RectangleMarker[] = [];
    const secs = sectionsOf(view.state);
    if (!secs.length) return out;
    const scroll = view.scrollDOM.getBoundingClientRect();
    const baseLeft = scroll.left - view.scrollDOM.scrollLeft * view.scaleX;
    const baseTop = scroll.top - view.scrollDOM.scrollTop * view.scaleY;
    const left = view.contentDOM.getBoundingClientRect().left - baseLeft;
    const { from: vFrom, to: vTo } = view.viewport;
    for (const s of secs) {
      if (s.bodyEnd <= s.headTo || isFolded(view.state, s) || s.bodyEnd < vFrom || s.headTo > vTo) continue;
      const first = view.lineBlockAt(s.headTo + 1);
      const last = view.lineBlockAt(s.bodyEnd);
      const top = view.documentTop + first.top - baseTop;
      const height = last.bottom - first.top;
      out.push(new RectangleMarker(`cm-sec-bar sec-h-${hlHue(s.color)}`, left, top, null, height));
    }
    return out;
  },
});

// ---- the cursor never rests inside "## {colour} " ----

const normalize = EditorState.transactionFilter.of((tr) => {
  if ((!tr.selection && !tr.docChanged) || tr.annotation(sectionOp) || tr.isUserEvent('select.search')) return tr;
  const secs = sectionsOf(tr.state);
  if (!secs.length) return tr;
  const old = tr.startState.selection.main.head;
  let changed = false;
  const fix = (pos: number, cursor: boolean) => {
    const s = secs.find((x) => x.from <= pos && pos < x.from + x.prefix);
    if (!s || (!cursor && pos === s.from)) return pos; // a range may start at the line start (whole-line selections)
    const end = s.from + s.prefix;
    // leaving leftwards from the name's start goes to the end of the line above
    if (old === end && pos < end && !tr.docChanged) return s.from > 0 ? s.from - 1 : end;
    return end;
  };
  const ranges = tr.newSelection.ranges.map((r) => {
    const head = fix(r.head, r.empty);
    const anchor = r.empty ? head : fix(r.anchor, false);
    if (head === r.head && anchor === r.anchor) return r;
    changed = true;
    return EditorSelection.range(anchor, head);
  });
  return changed ? [tr, { selection: EditorSelection.create(ranges, tr.newSelection.mainIndex), sequential: true }] : tr;
});

// ---- edits keep headings well-formed ----

/**
 * - an edit that cuts into "## {colour} " takes all of it (Backspace at the name's start turns the heading
 *   back into text, keeping the name); typing inside it lands at the name's start;
 * - Enter at the name's start opens a line above the heading;
 * - Backspace at the start of the first body line, or Delete at the end of the heading, doesn't merge the
 *   body into the name: the cursor moves across instead;
 * - a heading whose name is deleted becomes text (its "## {colour} " goes too).
 */
const guard = EditorState.transactionFilter.of((tr): TransactionSpec | readonly TransactionSpec[] => {
  if (!tr.docChanged || tr.annotation(sectionOp) || tr.annotation(imageOp) || tr.annotation(highlightOp)) return tr;
  const ev = tr.annotation(Transaction.userEvent);
  if (!ev || ev === 'input.replace') return tr;
  const start = tr.startState;
  const secs = sectionsOf(start);
  if (!secs.length) return tr;
  const doc = start.doc;
  const changes: { from: number; to: number; insert: string }[] = [];
  tr.changes.iterChanges((from, to, _fb, _tb, ins) => void changes.push({ from, to, insert: ins.toString() }));
  if (changes.length !== 1) return tr; // multi-range edits (search-replace etc.) go through as they are
  const c = changes[0];
  const sel = start.selection.main;

  // Backspace at a body's first line start / Delete at the heading end: move, don't merge
  if (c.insert === '' && c.to === c.from + 1 && sel.empty) {
    const s = secs.find((x) => x.headTo === c.from);
    if (s && c.to <= doc.length && doc.lineAt(c.to).text.trim() !== '') {
      if (tr.isUserEvent('delete.backward') && sel.head === c.to) return { selection: { anchor: s.headTo }, scrollIntoView: true };
      if (tr.isUserEvent('delete.forward') && sel.head === c.from) return { selection: { anchor: c.to }, scrollIntoView: true };
    }
  }

  const nameStart = (x: Section) => x.from + x.prefix;
  const inside = (p: number, x: Section) => p > x.from && p < nameStart(x);

  // Enter at the name's start: the new line goes above the heading
  if (c.from === c.to && c.insert === '\n' && sel.empty) {
    const h = secs.find((x) => nameStart(x) === c.from);
    if (h) return { changes: { from: h.from, insert: '\n' }, selection: { anchor: nameStart(h) + 1 }, userEvent: ev, scrollIntoView: true };
  }

  // Delete at the end of a line just above a heading doesn't pull the heading up into it
  if (c.insert === '' && c.to === c.from + 1 && sel.empty && tr.isUserEvent('delete.forward')) {
    const h = secs.find((x) => x.from === c.to);
    if (h && doc.lineAt(c.from).text.trim() !== '') return { selection: { anchor: nameStart(h) }, scrollIntoView: true };
  }

  const s = secs.find((x) => inside(c.from, x) || (c.to > c.from && inside(c.to, x)));
  if (s) {
    const end = nameStart(s);
    // typing inside the hidden part lands at the name's start
    if (c.from === c.to) return { changes: { from: end, insert: c.insert }, selection: { anchor: end + c.insert.length }, userEvent: ev, scrollIntoView: true };
    // a deletion reaching into it takes all of it (and whatever else it covered)
    const from = Math.min(c.from, s.from);
    const to = Math.max(c.to, end);
    return { changes: { from, to, insert: c.insert }, selection: { anchor: from + c.insert.length }, userEvent: ev, scrollIntoView: true };
  }

  // a heading left with no name stops being one: drop its "## {colour} "
  const after = tr.newDoc;
  for (const x of secs) {
    if (c.to < x.from || c.from > x.headTo) continue;
    const from = tr.changes.mapPos(x.from, 1);
    const line = after.lineAt(Math.min(from, after.length));
    const text = line.text;
    if (line.from === from && text.startsWith(`## {${x.color}} `) && !parseHeading(text) && text.slice(x.prefix).trim() === '') {
      return [tr, { changes: { from: line.from, to: line.from + Math.min(x.prefix, text.length) }, sequential: true }];
    }
  }
  return tr;
});

/** Search-and-replace never rewrites "## {colour} " (replacing "teal" can't recolour a section). */
const protectOnReplace = EditorState.changeFilter.of((tr) => {
  if (!tr.isUserEvent('input.replace')) return true;
  const out: number[] = [];
  for (const s of sectionsOf(tr.startState)) out.push(s.from, s.from + s.prefix);
  return out.length ? out : true;
});

// ---- commands ----

type View = Pick<EditorView, 'state' | 'dispatch'>;
const ops = [sectionOp.of(true), imageOp.of(true), highlightOp.of(true), isolateHistory.of('full')];
export const NEW_SECTION_NAME = 'New section';

/**
 * Start a section after the line the cursor is on (after a selected image; never above or on the title
 * line). A blank line becomes the heading. The name is selected, ready to type over.
 */
export function insertSection(view: View, color: string): boolean {
  const { state } = view;
  if (state.readOnly) return false;
  const doc = state.doc;
  const r = state.selection.main;
  let line = selectedImage(state) ?? doc.lineAt(r.head);
  const title = titleFrom(doc);
  if (title < 0) return false; // write a title first
  if (line.from <= title) line = doc.lineAt(title);
  const text = headingLine(color, NEW_SECTION_NAME);
  const nameAt = text.length - NEW_SECTION_NAME.length;
  let from: number, insert: string, at: number;
  if (line.from > title && line.text.trim() === '') {
    from = line.from;
    insert = text;
    at = line.from + nameAt;
    view.dispatch({ changes: { from, to: line.to, insert }, selection: { anchor: at, head: at + NEW_SECTION_NAME.length }, annotations: ops, userEvent: 'input.section', scrollIntoView: true });
    return true;
  }
  from = line.to;
  insert = '\n' + text;
  at = from + 1 + nameAt;
  view.dispatch({ changes: { from, insert }, selection: { anchor: at, head: at + NEW_SECTION_NAME.length }, annotations: ops, userEvent: 'input.section', scrollIntoView: true });
  return true;
}

/** Change a section's colour. */
export function recolorSection(view: View, index: number, color: string) {
  const s = sectionsOf(view.state)[index];
  if (!s || view.state.readOnly || s.color === color) return;
  view.dispatch({ changes: { from: s.from + 4, to: s.from + 4 + s.color.length, insert: color }, annotations: ops, userEvent: 'input.section' });
}

/** Turn a section's heading back into text (its name stays as a line; the body joins the section above). */
export function removeSection(view: View, index: number) {
  const s = sectionsOf(view.state)[index];
  if (!s || view.state.readOnly) return;
  const head = view.state.selection.main.head;
  view.dispatch({
    changes: { from: s.from, to: s.from + s.prefix },
    selection: head >= s.from && head <= s.headTo ? { anchor: Math.max(s.from, head - s.prefix) } : undefined,
    annotations: ops,
    userEvent: 'input.section',
  });
}

/**
 * Swap a section (heading + body) with the one above (dir -1) or below (+1). The text before the first
 * section never moves. One undo step; the cursor and collapsed state travel with the section.
 */
export function moveSection(view: View, index: number, dir: -1 | 1): boolean {
  const { state } = view;
  const secs = sectionsOf(state);
  const a = secs[dir < 0 ? index - 1 : index];
  const b = secs[dir < 0 ? index : index + 1];
  if (!a || !b || state.readOnly) return false;
  const doc = state.doc;
  const aText = doc.sliceString(a.from, a.end);
  const bText = doc.sliceString(b.from, b.end);
  // [a.from, b.end) holds "a\nb"; it becomes "b\na"
  const insert = bText + '\n' + aText;
  const moved = dir < 0 ? b : a; // the section being moved
  const head = state.selection.main.head;
  const inMoved = head >= moved.from && head <= moved.end;
  const newStart = dir < 0 ? a.from : a.from + bText.length + 1; // where the moved section now starts
  const cursor = inMoved ? newStart + (head - moved.from) : undefined;
  const foldA = isFolded(state, a);
  const foldB = isFolded(state, b);
  // folds inside the replaced text are dropped by the change; collapsed sections are folded again where
  // they land (effect positions are in the new document): b now sits at a.from, a after it
  const effects: StateEffect<unknown>[] = [];
  const bLen = b.bodyEnd - b.from;
  const aStart = a.from + bText.length + 1;
  const aLen = a.bodyEnd - a.from;
  if (foldB && b.bodyEnd > b.headTo) effects.push(foldEffect.of({ from: a.from + (b.headTo - b.from), to: a.from + bLen }));
  if (foldA && a.bodyEnd > a.headTo) effects.push(foldEffect.of({ from: aStart + (a.headTo - a.from), to: aStart + aLen }));
  view.dispatch({
    changes: { from: a.from, to: b.end, insert },
    selection: cursor === undefined ? undefined : { anchor: cursor },
    effects,
    annotations: ops,
    userEvent: 'move.section',
    scrollIntoView: true,
  });
  return true;
}

/** Put the cursor at the end of a section's heading (opening it if collapsed) and scroll it into view. */
export function gotoSection(view: EditorView, index: number) {
  const s = sectionsOf(view.state)[index];
  if (!s) return;
  view.focus();
  view.dispatch({
    selection: { anchor: s.headTo },
    effects: [...foldEffects(view.state, s, false), EditorView.scrollIntoView(s.from, { y: 'start', yMargin: 80 })],
    userEvent: 'select',
  });
}

export function sections(): Extension {
  return [headField, folding, drawer, bars, normalize, guard, protectOnReplace];
}
