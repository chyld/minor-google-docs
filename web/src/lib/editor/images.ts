// Editing rules for inline-image lines (`[image:<ref>] caption`), which markers.ts draws as block
// widgets. The model: an image is a block. The cursor only ever sits on text lines; an image is
// reached as an "image selection" (the whole line selected), and image lines change only as whole
// lines. See SPEC.md §17.
import {
  Annotation,
  EditorSelection,
  EditorState,
  Prec,
  Transaction,
  type Extension,
  type Line,
  type StateEffect,
  type Text,
  type TransactionSpec,
} from '@codemirror/state';
import { EditorView, ViewPlugin, type ViewUpdate } from '@codemirror/view';
import { MARKER_RE } from '../ids';
import { registry } from '../registry';

/** Our own structural edits: skip the guard and selection normalization. */
export const imageOp = Annotation.define<boolean>();

export function isImageLine(line: { text: string }): boolean {
  return line.text.startsWith('[image:') && MARKER_RE.test(line.text);
}

/** The selected image line, if the selection is exactly one whole image line. */
export function selectedImage(state: EditorState): Line | null {
  const { ranges, main } = state.selection;
  if (ranges.length !== 1 || main.empty) return null;
  const line = state.doc.lineAt(main.from);
  return main.from === line.from && main.to === line.to && isImageLine(line) ? line : null;
}

function imageAt(doc: Text, pos: number): Line | null {
  const line = doc.lineAt(pos);
  return isImageLine(line) ? line : null;
}

/** Nearest cursor position on a text line, looking first in direction dir (1 = below, -1 = above).
 * Below → start of that line, above → end of it. Null if the doc has no text line. */
export function textPos(doc: Text, from: Line, dir: 1 | -1): number | null {
  for (const d of [dir, -dir]) {
    for (let n = from.number + d; n >= 1 && n <= doc.lines; n += d) {
      const l = doc.line(n);
      if (!isImageLine(l)) return d > 0 ? l.from : l.to;
    }
  }
  return null;
}

/** The image line plus one separating newline (the following one, else the preceding one). */
export function imageLineRange(doc: Text, line: Line): { from: number; to: number } {
  if (line.to < doc.length) return { from: line.from, to: line.to + 1 };
  if (line.from > 0) return { from: line.from - 1, to: line.to };
  return { from: line.from, to: line.to };
}

function imageName(line: Line): string {
  return MARKER_RE.exec(line.text)?.[2] || 'image';
}

function announce(line: Line): StateEffect<string> {
  return EditorView.announce.of(`Image ${imageName(line)} selected. Space opens it, Backspace deletes it.`);
}

/** A selection that keeps every cursor off image lines. user: whether a person caused it
 * (then a cursor on an image selects that image; otherwise it moves to the nearest text). */
export function normalizeSelection(doc: Text, sel: EditorSelection, user: boolean, growRanges = true): EditorSelection | null {
  const r = sel.main;
  if (r.empty) {
    const img = imageAt(doc, r.head);
    if (!img) return null;
    const p = user ? null : textPos(doc, img, 1);
    return p === null ? EditorSelection.single(img.from, img.to) : EditorSelection.single(p);
  }
  if (!growRanges) return null;
  // a range ending inside an image line's hidden text grows to the whole line
  let { from, to } = r;
  const a = imageAt(doc, from);
  if (a && from > a.from && from < a.to) from = a.from;
  const b = imageAt(doc, to);
  if (b && to > b.from && to < b.to) to = b.to;
  // just one image plus its own line break(s) (e.g. select-line) means that image
  const only = imageAt(doc, Math.min(from + 1, doc.length));
  if (only && from >= only.from - 1 && to <= only.to + 1 && from <= only.from && to >= only.to && (from !== only.from || to !== only.to))
    return EditorSelection.single(only.from, only.to);
  if (from === r.from && to === r.to && sel.ranges.length === 1) return null;
  return EditorSelection.single(r.anchor <= r.head ? from : to, r.anchor <= r.head ? to : from);
}

export function initialSelection(doc: Text): EditorSelection {
  return normalizeSelection(doc, EditorSelection.single(0), false) ?? EditorSelection.single(0);
}

const userEventOf = (tr: Transaction) => tr.annotation(Transaction.userEvent);
// Search keeps its own match selections (a match in a caption must stay a match, or Replace stalls).
const isSearch = (tr: Transaction) => tr.isUserEvent('select.search') || tr.isUserEvent('input.replace');

/** Image lines change only as whole lines (see SPEC.md §17.2). */
const guard = EditorState.transactionFilter.of((tr): TransactionSpec | readonly TransactionSpec[] => {
  if (!tr.docChanged || tr.annotation(imageOp) || !userEventOf(tr)) return tr;
  const old = tr.startState.doc;
  const touched: Line[] = [];
  tr.changes.iterChangedRanges((fromA, toA) => {
    let pos = old.lineAt(Math.max(0, fromA - 1)).from;
    const end = Math.min(old.length, toA + 1);
    for (;;) {
      const l = old.lineAt(pos);
      if (isImageLine(l) && !touched.some((t) => t.from === l.from)) touched.push(l);
      if (l.to >= end || l.to >= old.length) break;
      pos = l.to + 1;
    }
  });
  // Image lines arriving inside inserted text (drag-and-drop, odd paths the paste filter can't see)
  // get their own line instead of being glued into the line they land in.
  const glue: { from: number; to: number; insert: string }[] = [];
  tr.changes.iterChanges((_fa, _ta, fromB, toB, inserted) => {
    if (!inserted.length) return;
    const nd = tr.newDoc;
    if (isImageLine(inserted.line(1)) && nd.lineAt(fromB).from !== fromB) glue.push({ from: fromB, to: fromB, insert: '\n' });
    if (isImageLine(inserted.line(inserted.lines)) && nd.lineAt(toB).to !== toB) glue.push({ from: toB, to: toB, insert: '\n' });
  });
  if (!touched.length) return glue.length ? [tr, { changes: glue, sequential: true }] : tr;

  const spans: { fromA: number; toA: number; text: string }[] = [];
  tr.changes.iterChanges((fromA, toA, _fb, _tb, inserted) => spans.push({ fromA, toA, text: inserted.toString() }));
  const doc = tr.newDoc;
  const extra: { from: number; to: number; insert?: string }[] = [];
  for (const m of touched) {
    const cover = spans.find((s) => s.fromA <= m.from && s.toA >= m.to);
    if (cover) {
      // "Just this image" = its text plus at most its own line breaks (Shift+arrow, select-line).
      const justImage = cover.fromA >= m.from - 1 && cover.toA <= m.to + 1;
      if (!justImage) continue; // deleted along with other content
      if (!cover.text && (cover.fromA !== m.from || cover.toA !== m.to)) {
        // the image and its line break(s) deleted: the neighbouring lines must not merge
        if (cover.fromA < m.from && cover.toA > m.to) extra.push({ from: tr.changes.mapPos(cover.fromA, -1), to: tr.changes.mapPos(cover.fromA, -1), insert: '\n' });
        continue;
      }
      if (cover.text) {
        // typing / IME / paste / Alt-t over a selected image: keep it, put the text on a new line after it
        if (spans.length !== 1) return {};
        const insert = '\n' + cover.text;
        return {
          changes: { from: m.to, insert },
          selection: { anchor: m.to + insert.length },
          annotations: [Transaction.userEvent.of(userEventOf(tr)!), imageOp.of(true)],
          scrollIntoView: true,
        };
      }
      // exactly the hidden text removed (e.g. cut): take one newline with it so no blank line is left
      const l = doc.lineAt(tr.changes.mapPos(m.from, -1));
      if (l.length === 0 && doc.lines > 1) extra.push(l.to < doc.length ? { from: l.to, to: l.to + 1 } : { from: l.from - 1, to: l.from });
      continue;
    }
    // the image survives: it must still be a whole, unchanged line
    const p = tr.changes.mapPos(m.from, 1);
    const l = doc.lineAt(p);
    if (l.from === p && l.text === m.text) continue;
    // text typed/pasted right up against the image (e.g. over a triple-clicked line above it)
    // keeps its own line; plain joins (nothing inserted) are refused
    const before = spans.find((s) => s.toA === m.from && s.fromA < m.from && s.text && !s.text.endsWith('\n'));
    const after = spans.find((s) => s.fromA === m.to && s.toA > m.to && s.text && !s.text.startsWith('\n'));
    const start = before ? p : l.from;
    const end = start + m.text.length;
    if (doc.sliceString(start, end) !== m.text || (!before && l.from !== p) || (!after && end !== doc.lineAt(end).to)) return {}; // veto
    if (before) extra.push({ from: p, to: p, insert: '\n' });
    if (after) extra.push({ from: end, to: end, insert: '\n' });
  }
  extra.push(...glue);
  return extra.length ? [tr, { changes: extra, sequential: true }] : tr;
});

/** Keeps cursors off image lines (see SPEC.md §17.3). Registered before the guard so it runs after it. */
const normalize = EditorState.transactionFilter.of((tr): TransactionSpec | readonly TransactionSpec[] => {
  if (tr.annotation(imageOp) || (!tr.docChanged && !tr.selection)) return tr;
  const sel = normalizeSelection(tr.newDoc, tr.newSelection, !!userEventOf(tr), !isSearch(tr));
  if (!sel) return tr;
  const img = sel.main.empty ? null : tr.newDoc.lineAt(sel.main.from);
  const effects = img && isImageLine(img) && sel.main.from === img.from && sel.main.to === img.to ? announce(img) : [];
  return [tr, { selection: sel, effects, sequential: true }];
});

/** Search-and-replace edits text but never the hidden text of image lines. */
const protectOnReplace = EditorState.changeFilter.of((tr) => {
  if (!tr.isUserEvent('input.replace')) return true;
  const out: number[] = [];
  const doc = tr.startState.doc;
  for (let n = 1; n <= doc.lines; n++) {
    const l = doc.line(n);
    if (isImageLine(l)) out.push(l.from, l.to);
  }
  return out.length ? out : true;
});

const isMarkerText = (s: string) => MARKER_RE.test(s);

/** Pasted text that starts or ends with an image line gets that line on its own. */
const pasteOwnLine = EditorView.clipboardInputFilter.of((text, state) => {
  const lines = text.split(/\r?\n/);
  const first = isMarkerText(lines[0]);
  const last = isMarkerText(lines[lines.length - 1]);
  if ((!first && !last) || selectedImage(state)) return text;
  const r = state.selection.main;
  const a = state.doc.lineAt(r.from);
  const b = state.doc.lineAt(r.to);
  return (first && r.from !== a.from ? '\n' : '') + text + (last && r.to !== b.to ? '\n' : '');
});

/** Open the lightbox for ref if its attachment is a ready image. */
export function openLightbox(ref: string) {
  const s = registry.get(ref);
  if (s.kind === 'ready' && s.att.inline) window.dispatchEvent(new CustomEvent('notes:lightbox', { detail: { ref } }));
}

export function selectImage(view: EditorView, line: Line, userEvent = 'select') {
  view.dispatch({
    selection: EditorSelection.single(line.from, line.to),
    userEvent,
    effects: announce(line),
    scrollIntoView: true,
    annotations: imageOp.of(true),
  });
}

function cursorTo(view: EditorView, pos: number) {
  view.dispatch({ selection: { anchor: pos }, userEvent: 'select', scrollIntoView: true });
}

/** Leave an image selection for typing: a new empty line right after the image. */
function escapeImage(view: EditorView) {
  const img = selectedImage(view.state);
  if (!img || view.state.readOnly) return;
  view.dispatch({
    changes: { from: img.to, insert: '\n' },
    selection: { anchor: img.to + 1 },
    userEvent: 'input',
    annotations: imageOp.of(true),
    scrollIntoView: true,
  });
}

function deleteImage(view: EditorView, img: Line, dir: 1 | -1) {
  const { doc } = view.state;
  const range = imageLineRange(doc, img);
  const changes = view.state.changes(range);
  const target = textPos(doc, img, dir);
  view.dispatch({
    changes,
    selection: { anchor: target === null ? range.from : changes.mapPos(target, dir > 0 ? 1 : -1) },
    userEvent: dir < 0 ? 'delete.backward' : 'delete.forward',
    annotations: imageOp.of(true),
    scrollIntoView: true,
  });
}

/** Move off a selected image: to the neighbouring image (selected) or text line. */
function stepFromImage(view: EditorView, img: Line, dir: 1 | -1) {
  const { doc } = view.state;
  const n = img.number + dir;
  if (n < 1 || n > doc.lines) return;
  const l = doc.line(n);
  if (isImageLine(l)) selectImage(view, l);
  else cursorTo(view, dir < 0 ? l.to : l.from);
}

const isMac = /Mac|iP(hone|ad|od)/.test(navigator.platform);

/** A key that will type a character. IME/virtual-keyboard keys (229, "Process") are left to
 * beforeinput/compositionstart, which know what they will do. */
function isTyping(e: KeyboardEvent): boolean {
  const altGr = e.getModifierState?.('AltGraph');
  if ((e.ctrlKey || e.metaKey) && !altGr) return false;
  if (e.altKey && !altGr && !isMac) return false; // Alt+key is a shortcut here, not a character
  return e.key.length === 1;
}

/** A non-empty selection with an end inside an image's hidden text (a search match in a caption). */
function partialImage(state: EditorState): boolean {
  const r = state.selection.main;
  if (r.empty) return false;
  return [r.from, r.to].some((pos) => {
    const l = imageAt(state.doc, pos);
    return !!l && pos > l.from && pos < l.to;
  });
}

function onKeydown(e: KeyboardEvent, view: EditorView): boolean {
  if (e.isComposing || view.composing) return false;
  const { state } = view;
  const ro = state.readOnly;
  const plain = !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey;
  const done = () => (e.preventDefault(), true);
  // Leaving a search match inside a caption: any editing key first selects the whole image.
  if (!e.ctrlKey && !e.metaKey && partialImage(state) && (isTyping(e) || ['Backspace', 'Delete', 'Enter', 'Tab'].includes(e.key))) {
    const sel = normalizeSelection(state.doc, state.selection, true)!;
    view.dispatch({ selection: sel, userEvent: 'select', annotations: imageOp.of(true), scrollIntoView: true });
    if (!isTyping(e)) return done();
    escapeImage(view); // a typed character still lands, on a new line after the image
    return false;
  }
  const img = selectedImage(state);

  if (img) {
    const ref = MARKER_RE.exec(img.text)![1];
    // Shift+arrows extend from the image over the whole neighbouring line, keeping the image whole
    if (e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey && e.key.startsWith('Arrow')) {
      const back = e.key === 'ArrowUp' || e.key === 'ArrowLeft';
      const n = img.number + (back ? -1 : 1);
      const nb = n >= 1 && n <= state.doc.lines ? state.doc.line(n) : null;
      const head = !nb ? (back ? img.from : img.to) : back ? nb.from : nb.to;
      view.dispatch({ selection: { anchor: back ? img.to : img.from, head }, userEvent: 'select', annotations: imageOp.of(true), scrollIntoView: true });
      return done();
    }
    switch (e.key) {
      case 'PageUp':
      case 'PageDown': {
        // step onto text, then let the normal page motion run from there
        const p = textPos(state.doc, img, e.key === 'PageUp' ? -1 : 1);
        if (p !== null) cursorTo(view, p);
        return false;
      }
      case ' ':
        if (!plain) break;
        if (!e.repeat) openLightbox(ref);
        return done();
      case 'ArrowUp':
      case 'ArrowLeft':
        if (!plain) return false;
        stepFromImage(view, img, -1);
        return done();
      case 'ArrowDown':
      case 'ArrowRight':
        if (!plain) return false;
        stepFromImage(view, img, 1);
        return done();
      case 'Backspace':
      case 'Delete':
        if (!e.repeat && !ro) deleteImage(view, img, e.key === 'Backspace' ? -1 : 1);
        return done();
      case 'Enter': {
        if (ro) return done();
        if (e.ctrlKey || e.metaKey || e.altKey) return false; // e.g. Mod-Enter: insertBlankLine
        const at = e.shiftKey ? img.from : img.to;
        view.dispatch({
          changes: { from: at, insert: '\n' },
          selection: { anchor: e.shiftKey ? img.from : img.to + 1 },
          userEvent: 'input',
          annotations: imageOp.of(true),
          scrollIntoView: true,
        });
        return done();
      }
      case 'Escape': {
        const p = textPos(state.doc, img, 1);
        if (p === null) return false;
        cursorTo(view, p);
        return done();
      }
    }
    if (isTyping(e)) escapeImage(view); // the key's own input then lands on the new line
    return false;
  }

  const r = state.selection.main;
  if (!plain || !r.empty || state.selection.ranges.length > 1) return false;
  const doc = state.doc;
  const L = doc.lineAt(r.head);
  const prev = L.number > 1 ? doc.line(L.number - 1) : null;
  const next = L.number < doc.lines ? doc.line(L.number + 1) : null;
  const prevImg = prev && isImageLine(prev) ? prev : null;
  const nextImg = next && isImageLine(next) ? next : null;
  switch (e.key) {
    case 'ArrowUp':
      if (prevImg && doc.lineAt(view.moveVertically(r, false).head).number !== L.number) return selectImage(view, prevImg), done();
      return false;
    case 'ArrowDown':
      if (nextImg && doc.lineAt(view.moveVertically(r, true).head).number !== L.number) return selectImage(view, nextImg), done();
      return false;
    case 'ArrowLeft':
      if (prevImg && r.head === L.from) return selectImage(view, prevImg), done();
      return false;
    case 'ArrowRight':
      if (nextImg && r.head === L.to) return selectImage(view, nextImg), done();
      return false;
    case 'Backspace':
      // From text, the first press only selects the image. An empty line just goes (the default
      // join, which then selects the image) — but a held key stops here instead.
      if (!prevImg || r.head !== L.from) return false;
      if (e.repeat) return done();
      if (L.length === 0) return false;
      selectImage(view, prevImg);
      return done();
    case 'Delete':
      if (!nextImg || r.head !== L.to) return false;
      if (e.repeat) return done();
      if (L.length === 0) return false;
      selectImage(view, nextImg);
      return done();
  }
  return false;
}

const handlers = Prec.highest(
  EditorView.domEventHandlers({
    keydown: onKeydown,
    beforeinput(e, view) {
      const img = selectedImage(view.state);
      if (!img || view.state.readOnly) return false;
      // virtual keyboards send Backspace/Delete only as input events
      if (e.inputType === 'deleteContentBackward' || e.inputType === 'deleteContentForward') {
        e.preventDefault();
        deleteImage(view, img, e.inputType === 'deleteContentBackward' ? -1 : 1);
        return true;
      }
      if (!e.inputType.startsWith('insert')) return false;
      escapeImage(view);
      if (e.inputType === 'insertParagraph' || e.inputType === 'insertLineBreak') {
        e.preventDefault();
        return true;
      }
      return false;
    },
    paste(e, view) {
      // a copied image pasted at the very top still goes after the title, never above it
      const text = e.clipboardData?.getData('text/plain') ?? '';
      const r = view.state.selection.main;
      if (!r.empty || !isMarkerText(text.split(/\r?\n/)[0])) return false;
      const doc = view.state.doc;
      for (let n = 1; n <= doc.lines; n++) {
        const l = doc.line(n);
        if (isImageLine(l) || !l.text.trim()) continue;
        if (r.from <= l.from) view.dispatch({ selection: { anchor: l.to }, annotations: imageOp.of(true) });
        break;
      }
      return false; // the normal paste runs from there
    },
    compositionstart(_e, view) {
      escapeImage(view);
      return false;
    },
  }),
);

/** Marks the selected image's widget, and in read-only mode makes widgets keyboard-openable. */
const selectedMark = ViewPlugin.fromClass(
  class {
    constructor(view: EditorView) {
      queueMicrotask(() => this.sync(view));
    }
    update(u: ViewUpdate) {
      if (u.selectionSet || u.docChanged || u.viewportChanged || u.focusChanged || u.transactions.length === 0) this.sync(u.view);
    }
    sync(view: EditorView) {
      const img = selectedImage(view.state);
      const ro = view.state.readOnly;
      for (const fig of view.contentDOM.querySelectorAll<HTMLElement>('figure.cm-img')) {
        let pos = -1;
        try {
          pos = view.posAtDOM(fig);
        } catch {
          // detached
        }
        // the selected image, or one holding a search match in its caption
        const r = view.state.selection.main;
        const hit = !img && !r.empty && pos >= 0 && r.from >= pos && r.to <= view.state.doc.lineAt(pos).to && r.from < view.state.doc.lineAt(pos).to;
        fig.classList.toggle('selected', (!!img && pos === img.from) || hit);
        if (ro) {
          fig.tabIndex = 0;
          fig.setAttribute('role', 'button');
        } else {
          fig.removeAttribute('tabindex');
          fig.setAttribute('role', 'img');
        }
      }
    }
  },
);

/** Paths that bypass transaction filters (new state on load, undo/redo): fix up afterwards. */
const fallback = EditorView.updateListener.of((u) => {
  if (!u.selectionSet && !u.docChanged) return;
  const user = u.transactions.some((t) => !!userEventOf(t));
  const sel = normalizeSelection(u.state.doc, u.state.selection, user, !u.transactions.some(isSearch));
  if (!sel) return;
  queueMicrotask(() => {
    if (u.view.state !== u.state) return; // something else already moved on
    u.view.dispatch({ selection: sel, annotations: imageOp.of(true) });
  });
});

export function imageEditing(): Extension {
  return [normalize, guard, protectOnReplace, pasteOwnLine, handlers, selectedMark, fallback];
}
