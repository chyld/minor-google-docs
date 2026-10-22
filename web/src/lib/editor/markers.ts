// CodeMirror extension: renders `[image:<ref>] caption` lines as inline images
// (block widgets, atomic so the cursor skips them and Backspace removes the line),
// and styles the first text line as the note title.
import { EditorState, type Extension, RangeSet, StateField, type Range, type Transaction } from '@codemirror/state';
import { Decoration, type DecorationSet, EditorView, WidgetType } from '@codemirror/view';
import { MARKER_RE } from '../ids';
import { attachmentURL } from '../api';
import { registry, type RefState } from '../registry';

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
    return 220;
  }

  toDOM(): HTMLElement {
    const fig = document.createElement('figure');
    fig.className = 'cm-img';
    fig.setAttribute('contenteditable', 'false');
    const render = (s: RefState) => {
      fig.replaceChildren();
      fig.dataset.state = s.kind;
      if (s.kind === 'loading') {
        fig.append(el('div', 'cm-img-skeleton'));
      } else if (s.kind === 'missing') {
        const box = el('div', 'cm-img-missing');
        box.textContent = `Image not found · ${this.caption || this.ref}`;
        fig.append(box);
      } else {
        const btn = el('button', 'cm-img-frame') as HTMLButtonElement;
        btn.type = 'button';
        btn.title = 'Open image';
        const img = document.createElement('img');
        img.alt = this.caption || 'image';
        img.loading = 'lazy';
        img.decoding = 'async';
        if (s.kind === 'pending') {
          img.src = s.url;
          const ring = el('div', 'cm-img-progress');
          ring.style.setProperty('--p', String(Math.round(s.progress * 100)));
          ring.textContent = s.error ? '!' : `${Math.round(s.progress * 100)}%`;
          btn.append(img, ring);
          if (s.error) fig.dataset.state = 'error';
        } else {
          if (!s.att.inline) {
            const box = el('div', 'cm-img-missing');
            box.textContent = `Not an image · ${s.att.filename}`;
            fig.append(box);
            return;
          }
          img.src = attachmentURL(s.att.id);
          img.onerror = () => {
            fig.dataset.state = 'broken';
          };
          btn.append(img);
          btn.onclick = () => window.dispatchEvent(new CustomEvent('notes:lightbox', { detail: { ref: this.ref } }));
        }
        fig.append(btn);
        if (this.caption) {
          const cap = el('figcaption', 'cm-img-caption');
          cap.textContent = this.caption;
          fig.append(cap);
        }
      }
    };
    (fig as any)._unsub = registry.subscribe(this.ref, render);
    return fig;
  }

  destroy(dom: HTMLElement) {
    (dom as any)._unsub?.();
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
