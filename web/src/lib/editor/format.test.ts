// Bold and underline: a highlight's style set (SPEC §19).
import { describe, expect, it } from 'vitest';
import { EditorSelection, EditorState, type TransactionSpec } from '@codemirror/state';
import { history, undo } from '@codemirror/commands';
import { markers } from './markers';
import { imageEditing } from './images';
import { applyStyle, cleanSlice, highlightAt, highlights, styleState, type StyleOp } from './highlights';
import { stripHighlights } from '../highlight';

const IMG = '[image:aaaaaaaaaa] one.png';
function mk(doc: string, anchor = 0, head = anchor) {
  return EditorState.create({ doc, selection: EditorSelection.single(anchor, head), extensions: [history(), markers(), highlights(), imageEditing()] });
}
function run(s: EditorState, op: StyleOp) {
  let out = s;
  const done = applyStyle({ state: s, dispatch: (tr: any) => (out = tr.state ?? s.update(tr).state) } as any, op);
  return { s: out, done };
}
const sel = (doc: string, text: string) => [doc.indexOf(text), doc.indexOf(text) + text.length] as const;
const B: StyleOp = { kind: 'toggle', style: 'b' };
const U: StyleOp = { kind: 'toggle', style: 'u' };

describe('bold and underline', () => {
  it('bolds the selection, and the same toggle takes it off again (one undo step each)', () => {
    const d = 'Title\nthe server ip';
    const a = run(mk(d, ...sel(d, 'server')), B).s;
    expect(a.doc.toString()).toBe('Title\nthe =={b}server== ip');
    expect(styleState(a).bold).toBe('all');
    const b = run(a, B).s;
    expect(b.doc.toString()).toBe(d);
    let back = a;
    undo({ state: a, dispatch: (tr) => (back = tr.state) });
    expect(back.doc.toString()).toBe(d);
  });

  it('combines with a colour and with underline in one span, in canonical order', () => {
    const d = 'T\nx =={coral}word== y';
    const s1 = run(mk(d, ...sel(d, 'word')), U).s;
    expect(s1.doc.toString()).toBe('T\nx =={coral+u}word== y');
    const s2 = run(s1, B).s;
    expect(s2.doc.toString()).toBe('T\nx =={coral+b+u}word== y');
  });

  it('a mixed selection becomes all bold; decided once for every line', () => {
    const d = 'T\n=={b}one==\ntwo';
    const s = run(mk(d, d.indexOf('one'), d.length), B).s;
    expect(s.doc.toString()).toBe('T\n=={b}one==\n=={b}two==');
    // now all bold: one toggle removes it everywhere
    const s2 = run(s, B).s;
    expect(s2.doc.toString()).toBe('T\none\ntwo');
  });

  it('changing the colour keeps bold, per character', () => {
    const d = 'T\n=={b}ab== cd';
    const s = run(mk(d, d.indexOf('ab') , d.length), { kind: 'color', name: 'sky' }).s;
    expect(s.doc.toString()).toBe('T\n=={sky+b}ab===={sky} cd==');
  });

  it('removing the colour keeps bold and underline; clear removes everything', () => {
    const d = 'T\n=={coral+b+u}word==';
    const [a, b] = sel(d, 'word');
    expect(run(mk(d, a, b), { kind: 'color', name: null }).s.doc.toString()).toBe('T\n=={b+u}word==');
    expect(run(mk(d, a, b), { kind: 'clear' }).s.doc.toString()).toBe('T\nword');
  });

  it('a cursor inside a span toggles the whole span (untrimmed); in plain text it does nothing', () => {
    const d = 'T\nx =={coral} two words == y';
    const s = run(mk(d, d.indexOf('words')), B).s;
    expect(s.doc.toString()).toBe('T\nx =={coral+b} two words == y');
    const plain = run(mk('T\nplain text', 8), B);
    expect(plain.done).toBe(false);
  });

  it('skips image lines and section headings; trims when adding, not when removing', () => {
    const d = `T\n  one  \n${IMG}\n## {teal} Head\ntwo`;
    const s = run(mk(d, d.indexOf('one') - 2, d.length), B).s;
    expect(s.doc.toString()).toBe(`T\n  =={b}one==  \n${IMG}\n## {teal} Head\n=={b}two==`);
    const d2 = 'T\n=={b} spaced ==';
    expect(run(mk(d2, d2.indexOf(' spaced'), d2.indexOf(' ==') + 1), B).s.doc.toString()).toBe('T\n spaced ');
  });

  it('never splits an emoji, and the text without delimiters never changes', () => {
    const d = 'T\nsmile 😀 now';
    const s = run(mk(d, d.indexOf('😀') + 1, d.length), U).s; // starts inside the emoji
    expect(s.doc.toString()).toBe('T\nsmile =={u}😀 now==');
    let seed = 3;
    const rnd = (n: number) => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) % n);
    const ops: StyleOp[] = [B, U, { kind: 'clear' }, { kind: 'color', name: 'mint' }, { kind: 'color', name: null }];
    let st = mk('T\nalpha beta == gamma= delta');
    for (let k = 0; k < 300; k++) {
      const len = st.doc.length;
      const a = 2 + rnd(len - 2);
      const b = a + rnd(len - a + 1);
      st = run(st.update({ selection: { anchor: a, head: b } }).state, ops[rnd(ops.length)]).s;
      expect(stripHighlights(st.doc.toString())).toBe('T\nalpha beta == gamma= delta');
    }
  });

  it('reports the state the buttons show', () => {
    const d = 'T\n=={b}ab== cd =={coral+u}ef==';
    const st = styleState(mk(d, d.indexOf('ab'), d.length));
    expect(st).toMatchObject({ bold: 'some', underline: 'some', hasColor: true, styled: true, color: null });
    expect(styleState(mk(d, ...sel(d, 'ab')))).toMatchObject({ bold: 'all', underline: 'none', hasColor: false });
  });

  it('a read-only note is never changed', () => {
    const s = EditorState.create({ doc: 'T\nword', selection: EditorSelection.single(2, 6), extensions: [highlights(), EditorState.readOnly.of(true)] });
    expect(run(s, B).done).toBe(false);
  });
});

describe('review fixes', () => {
  it('a bare cursor keeps its place in the text, so Ctrl+B then Ctrl+U build one span', () => {
    const d = 'T\nx =={coral}word== y';
    const at = d.indexOf('word') + 2; // between "wo" and "rd"
    const a = run(mk(d, at), B).s;
    expect(a.doc.toString()).toBe('T\nx =={coral+b}word== y');
    expect(a.sliceDoc(a.selection.main.head - 2, a.selection.main.head + 2)).toBe('word');
    const b = run(a, U).s;
    expect(b.doc.toString()).toBe('T\nx =={coral+b+u}word== y');
    expect(highlightAt(b)?.name).toBe('coral+b+u');
  });

  it('text with "==" and a trailing "=" can be toggled on and off again', () => {
    for (const text of ['a == b', 'x=']) {
      const d = `T\n${text}`;
      const on = run(mk(d, 2, d.length), B).s;
      expect(styleState(on.update({ selection: { anchor: 2, head: on.doc.length } }).state).bold).toBe('all');
      const off = run(on.update({ selection: { anchor: 2, head: on.doc.length } }).state, B).s;
      expect(off.doc.toString()).toBe(d);
    }
  });

  it('a selection across a section heading formats the text around it', () => {
    const d = 'T\none\n## {teal} Head\ntwo';
    const s = run(mk(d, d.indexOf('one'), d.length), B).s;
    expect(s.doc.toString()).toBe('T\n=={b}one==\n## {teal} Head\n=={b}two==');
  });

  it('picking a colour with the cursor in a span of only spaces leaves it alone', () => {
    const d = 'T\nx =={coral} == y';
    const s = run(mk(d, d.indexOf('} ') + 2), { kind: 'color', name: 'sky' }).s;
    expect(s.doc.toString()).toBe(d);
  });

  it('a scan limit gives "mixed", never invented colours', () => {
    const big = 'T\n' + 'word '.repeat(20);
    const st = styleState(mk(big, 2, big.length), 10);
    expect(st).toMatchObject({ bold: 'some', exact: false });
  });
});

// The existing edit rules hold for combined names (longer openers shift every offset).
const apply = (s: EditorState, spec: TransactionSpec) => s.update(spec).state;
const type = (s: EditorState, from: number, insert: string, to = from, userEvent = 'input.type') =>
  apply(s, { changes: { from, to, insert }, selection: { anchor: from + insert.length }, userEvent });
for (const name of ['b', 'coral+b', 'b+u', 'sky-deep+b+u']) {
  describe(`edit rules with =={${name}}`, () => {
    const d = `x =={${name}}abcd== y`;
    const inner = d.indexOf('abcd');
    it('Enter in the middle closes and reopens it', () => {
      const s = type(mk(d, inner + 2), inner + 2, '\n', inner + 2, 'input');
      expect(s.doc.toString()).toBe(`x =={${name}}ab==\n=={${name}}cd== y`);
    });
    it('Backspace on the closer unwraps it, keeping the text', () => {
      const end = inner + 4 + 2;
      expect(apply(mk(d, end), { changes: { from: end - 1, to: end }, selection: { anchor: end - 1 }, userEvent: 'delete.backward' }).doc.toString()).toBe('x abcd y');
    });
    it('typing "==" inside splits it', () => {
      let s = type(mk(d, inner + 2), inner + 2, '=');
      s = type(s, s.selection.main.head, '=');
      expect(stripHighlights(s.doc.toString())).toBe('x ab==cd y');
      expect(s.doc.toString()).toBe(`x =={${name}}ab======{${name}}cd== y`);
    });
    it('replace-all never edits a delimiter', () => {
      const changes = [...d.matchAll(/ab/g)].map((m) => ({ from: m.index!, to: m.index! + 2, insert: 'Z' }));
      expect(apply(mk(d), { changes, userEvent: 'input.replace.all' }).doc.toString()).toBe(`x =={${name}}Zcd== y`);
    });
    it('copying part of it gives plain text, and balanced raw text for this app', () => {
      expect(cleanSlice(mk(d), inner + 2, d.length)).toEqual({ plain: 'cd y', raw: `=={${name}}cd== y` });
    });
  });
}
