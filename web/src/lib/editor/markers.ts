// CodeMirror extension: renders `[image:<ref>] caption` lines as inline images
// (block widgets, atomic), and styles the first text line as the note title.
// Editing rules for image lines live in images.ts.
import { EditorState, type Extension, RangeSet, StateField, type Range, type Transaction } from '@codemirror/state';
import { Decoration, type DecorationSet, EditorView, WidgetType } from '@codemirror/view';
import { MARKER_RE } from '../ids';
import { attachmentURL } from '../api';
import { registry, type RefState } from '../registry';
import { openLightbox, selectImage, selectedImage } from './images';

// Natural image sizes seen this session, so a re-created widget gets its box right before load.
const sizes = new Map<string, { w: number; h: number }>();
function rememberSize(ref: string, img: HTMLImageElement) {
  if (!img.naturalWidth || !img.naturalHeight) return;
  sizes.delete(ref); // most recently used last
  if (sizes.size >= 500) sizes.delete(sizes.keys().next().value!);
  sizes.set(ref, { w: img.naturalWidth, h: img.naturalHeight });
}

class ImageWidget extends WidgetType {
  constructor(
    readonly ref: string,
    readonly caption: string,
  ) {
    super();
  }

  eq(o: ImageWidget) {
    return o.ref === this.ref && o.caption === this.caption;
  }

  get estimatedHeight() {
    const s = sizes.get(this.ref);
    if (!s) return 220;
    return Math.min(520, (680 * s.h) / s.w) + (this.caption ? 22 : 0) + 22;
  }

  toDOM(view: EditorView): HTMLElement {
    const fig = document.createElement('figure');
    fig.className = 'cm-img';
    fig.setAttribute('contenteditable', 'false');
    fig.setAttribute('role', 'img');
    fig.setAttribute('aria-label', this.caption || 'image');

    // Pending uploads update progress in place instead of rebuilding the DOM on every tick.
    let ring: HTMLElement | null = null;
    let pendingURL = '';
    const render = (s: RefState) => {
      if (s.kind === 'pending' && ring && s.url === pendingURL) {
        ring.style.setProperty('--p', String(Math.round(s.progress * 100)));
        ring.textContent = s.error ? '!' : `${Math.round(s.progress * 100)}%`;
        fig.dataset.state = s.error ? 'error' : 'pending';
        return;
      }
      ring = null;
      pendingURL = '';
      fig.replaceChildren();
      fig.dataset.state = s.kind;
      if (s.kind === 'loading') {
        fig.append(el('div', 'cm-img-skeleton'));
      } else if (s.kind === 'missing') {
        const box = el('div', 'cm-img-missing');
        box.textContent = `Image not found · ${this.caption || this.ref}`;
        fig.append(box);
      } else {
        if (s.kind === 'ready' && !s.att.inline) {
          const box = el('div', 'cm-img-missing');
          box.textContent = `Not an image · ${s.att.filename}`;
          fig.append(box);
          return;
        }
        const frame = el('div', 'cm-img-frame');
        const img = document.createElement('img');
        img.alt = this.caption || 'image';
        img.decoding = 'async';
        img.draggable = false;
        const known = sizes.get(this.ref);
        if (known) img.style.aspectRatio = `${known.w} / ${known.h}`;
        img.onload = () => rememberSize(this.ref, img);
        if (s.kind === 'pending') {
          img.src = s.url;
          ring = el('div', 'cm-img-progress');
          pendingURL = s.url;
          frame.append(img, ring);
          render(s);
        } else {
          img.src = attachmentURL(s.att.id);
          img.onerror = () => {
            fig.dataset.state = 'broken';
          };
          frame.title = 'Click to select · click again to open';
          frame.append(img);
        }
        fig.append(frame);
        if (this.caption) {
          const cap = el('figcaption', 'cm-img-caption');
          cap.textContent = this.caption;
          fig.append(cap);
        }
      }
    };
    const unsub = registry.subscribe(this.ref, render);

    // Report size changes (image load, state swaps) so CodeMirror's height map stays true.
    let last = '';
    const ro = new ResizeObserver(([entry]) => {
      const size = `${entry.contentRect.width}x${entry.contentRect.height}`;
      if (size === last) return;
      last = size;
      if (!view.dom.isConnected) return;
      view.requestMeasure();
    });
    ro.observe(fig);

    // Mouse: first click selects the image, a click on the selected image opens it.
    // In read-only mode a click opens it straight away.
    let wasSelected = false;
    let down: { x: number; y: number } | null = null;
    const lineHere = () => view.state.doc.lineAt(view.posAtDOM(fig));
    fig.addEventListener('mousedown', (e) => {
      if (e.button !== 0) return;
      const line = lineHere();
      if (e.shiftKey && !view.state.readOnly) {
        e.preventDefault();
        const a = view.state.selection.main.anchor;
        view.focus();
        view.dispatch({ selection: { anchor: a, head: a <= line.from ? line.to : line.from }, userEvent: 'select.pointer' });
        return;
      }
      if (e.ctrlKey || e.metaKey || e.altKey) return;
      e.preventDefault();
      down = { x: e.clientX, y: e.clientY };
      const sel = selectedImage(view.state);
      wasSelected = !!sel && sel.from === line.from;
      if (view.state.readOnly) return;
      view.focus();
      if (!wasSelected) selectImage(view, line, 'select.pointer');
    });
    fig.addEventListener('click', (e) => {
      if (e.button !== 0 || !down) return;
      const moved = Math.abs(e.clientX - down.x) + Math.abs(e.clientY - down.y) > 6;
      down = null;
      if (!moved && (wasSelected || view.state.readOnly)) openLightbox(this.ref);
    });
    fig.addEventListener('keydown', (e) => {
      if (view.state.readOnly && (e.key === 'Enter' || e.key === ' ')) {
        e.preventDefault();
        openLightbox(this.ref);
      }
    });

    (fig as any)._cleanup = () => {
      unsub();
      ro.disconnect();
    };
    return fig;
  }

  destroy(dom: HTMLElement) {
    (dom as any)._cleanup?.();
  }

  ignoreEvent() {
    return true;
  }
}

function el(tag: string, cls: string) {
  const e = document.createElement(tag);
  e.className = cls;
  return e;
}

const titleLine = Decoration.line({ class: 'cm-title-line' });

function build(state: EditorState): DecorationSet {
  const decos: Range<Decoration>[] = [];
  let titleDone = false;
  const doc = state.doc;
  for (let i = 1; i <= doc.lines; i++) {
    const line = doc.line(i);
    const text = line.text;
    if (text.startsWith('[image:')) {
      const m = MARKER_RE.exec(text);
      if (m) {
        decos.push(Decoration.replace({ widget: new ImageWidget(m[1], m[2] ?? ''), block: true }).range(line.from, line.to));
        continue;
      }
    }
    if (!titleDone && text.trim() !== '') {
      decos.push(titleLine.range(line.from));
      titleDone = true;
    }
  }
  return Decoration.set(decos, true);
}

/** True if a change touches the title line or any marker line (before or after). */
function needsRebuild(tr: Transaction): boolean {
  const old = tr.startState.doc;
  const doc = tr.state.doc;
  const titleEnd = firstTextLineEnd(old);
  let rebuild = false;
  tr.changes.iterChanges((fromA, toA, fromB, toB, inserted) => {
    if (rebuild) return;
    if (fromA <= titleEnd || inserted.toString().includes('[image:')) return void (rebuild = true);
    for (let p = old.lineAt(fromA).from; p <= toA; ) {
      const l = old.lineAt(p);
      if (l.text.startsWith('[image:')) return void (rebuild = true);
      p = l.to + 1;
    }
    for (let p = doc.lineAt(fromB).from; p <= toB; ) {
      const l = doc.lineAt(p);
      if (l.text.startsWith('[image:')) return void (rebuild = true);
      p = l.to + 1;
    }
  });
  return rebuild;
}

function firstTextLineEnd(doc: EditorState['doc']): number {
  for (let i = 1; i <= doc.lines; i++) {
    const l = doc.line(i);
    if (l.text.trim() !== '' && !MARKER_RE.test(l.text)) return l.to + 1;
  }
  return doc.length;
}

const markerField = StateField.define<DecorationSet>({
  create: build,
  update(deco, tr) {
    if (!tr.docChanged) return deco;
    // typing in ordinary lines just shifts existing decorations (no full rescan)
    return needsRebuild(tr) ? build(tr.state) : deco.map(tr.changes);
  },
  provide: (f) => [
    EditorView.decorations.from(f),
    EditorView.atomicRanges.of((view) => {
      const set = view.state.field(f);
      // only the replace (widget) ranges are atomic, not the title line
      const ranges: Range<Decoration>[] = [];
      set.between(0, view.state.doc.length, (from, to, d) => {
        if (from !== to) ranges.push(d.range(from, to));
      });
      return RangeSet.of(ranges);
    }),
  ],
});

export function markers(): Extension {
  return markerField;
}

/** Returns all refs referenced by markers in the text. */
export function refsIn(text: string): Set<string> {
  const out = new Set<string>();
  for (const line of text.split('\n')) {
    const m = MARKER_RE.exec(line);
    if (m) out.add(m[1]);
  }
  return out;
}
