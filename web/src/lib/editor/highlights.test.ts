import { describe, expect, it } from 'vitest';
import { EditorSelection, EditorState, type TransactionSpec } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { history, undo } from '@codemirror/commands';
import { markers } from './markers';
import { imageEditing } from './images';
import { applyHighlight, cleanSlice, highlightAt, highlights } from './highlights';
import { stripHighlights } from '../highlight';

const IMG = '[image:aaaaaaaaaa] one.png';

function mk(doc: string, anchor = 0, head = anchor) {
  return EditorState.create({ doc, selection: EditorSelection.single(anchor, head), extensions: [history(), markers(), highlights(), imageEditing()] }); // same order as Editor.svelte
}
const apply = (s: EditorState, spec: TransactionSpec) => s.update(spec).state;
/** Run applyHighlight on a state; returns the new state. */
function hl(s: EditorState, name: string | null) {
  let out = s;
  applyHighlight({ state: s, dispatch: (tr: any) => (out = tr.state ?? s.update(tr).state) } as any, name);
  return out;
}
/** Select [from, to) given as substrings of the doc. */
const sel = (doc: string, text: string) => [doc.indexOf(text), doc.indexOf(text) + text.length] as const;
/** User typing at pos (insert, or replace [pos, to)). */
const type = (s: EditorState, from: number, insert: string, to = from, userEvent = 'input.type') =>
  apply(s, { changes: { from, to, insert }, selection: { anchor: from + insert.length }, userEvent });

describe('applying and removing', () => {
  it('wraps the selection; one undo step takes it away', () => {
    const d = 'Title\nthe server ip is here';
    const s = hl(mk(d, ...sel(d, 'server ip')), 'coral');
    expect(s.doc.toString()).toBe('Title\nthe =={coral}server ip== is here');
    const r = s.selection.main;
    expect(s.sliceDoc(r.from, r.to)).toBe('server ip'); // selection = the coloured text
    let back = s;
    undo({ state: s, dispatch: (tr) => (back = tr.state) });
    expect(back.doc.toString()).toBe(d);
  });

  it('is idempotent', () => {
    const d = 'x =={sky}word== y';
    const s0 = mk(d, d.indexOf('word'), d.indexOf('word') + 4);
    expect(hl(s0, 'sky').doc.toString()).toBe(d);
  });

  it('trims the selection to its non-whitespace extent', () => {
    const d = 'a  word  b';
    expect(hl(mk(d, 1, 9), 'mint').doc.toString()).toBe('a  =={mint}word==  b');
  });

  it('works line by line and skips image lines and blank lines', () => {
    const d = `T\none\n${IMG}\n\ntwo`;
    const s = hl(mk(d, d.indexOf('one'), d.length), 'amber');
    expect(s.doc.toString()).toBe(`T\n=={amber}one==\n${IMG}\n\n=={amber}two==`);
  });

  it('recolouring a part splits the highlight; the rest keeps its colour', () => {
    const d = 'x =={coral}alpha beta gamma== y';
    const s = hl(mk(d, ...sel(d, 'beta')), 'blue-deep');
    expect(s.doc.toString()).toBe('x =={coral}alpha ===={blue-deep}beta===={coral} gamma== y');
    expect(stripHighlights(s.doc.toString())).toBe('x alpha beta gamma y');
  });

  it('touching highlights of the same colour merge', () => {
    const d = '=={lime}ab==cd';
    expect(hl(mk(d, ...sel(d, 'cd')), 'lime').doc.toString()).toBe('=={lime}abcd==');
  });

  it('with only a cursor inside, recolours or removes that highlight', () => {
    const d = 'x =={coral}word== y';
    const at = d.indexOf('word') + 1;
    expect(hl(mk(d, at), 'sky').doc.toString()).toBe('x =={sky}word== y');
    expect(hl(mk(d, at), null).doc.toString()).toBe('x word y');
    expect(hl(mk('plain', 2), 'sky').doc.toString()).toBe('plain'); // nothing to do
  });

  it('removing part of a highlight unwraps only that part', () => {
    const d = '=={pink}one two three==';
    expect(hl(mk(d, ...sel(d, 'two')), null).doc.toString()).toBe('=={pink}one ==two=={pink} three==');
  });

  it('text containing "==" is highlighted around it, never broken', () => {
    const d = 'if a == b then';
    const s = hl(mk(d, 0, d.length), 'teal');
    expect(stripHighlights(s.doc.toString())).toBe(d);
  });

  it('read-only notes are never changed', () => {
    const s = EditorState.create({ doc: 'abc', selection: EditorSelection.single(0, 3), extensions: [highlights(), EditorState.readOnly.of(true)] });
    expect(hl(s, 'coral').doc.toString()).toBe('abc');
  });
});

describe('editing keeps highlights well-formed', () => {
  const d = 'x =={coral}abcd== y';
  const inner = d.indexOf('abcd');

  it('typing inside extends the highlight', () => {
    const s = type(mk(d, inner + 2), inner + 2, 'ZZ');
    expect(s.doc.toString()).toBe('x =={coral}abZZcd== y');
  });

  it('Enter in the middle closes and reopens it on each line, cursor inside the new one', () => {
    const s = type(mk(d, inner + 2), inner + 2, '\n', inner + 2, 'input');
    expect(s.doc.toString()).toBe('x =={coral}ab==\n=={coral}cd== y');
    expect(highlightAt(s)?.name).toBe('coral');
    const line2 = s.doc.line(2);
    expect(s.selection.main.head).toBe(line2.from + '=={coral}'.length);
  });

  it('Enter at the end leaves the new line plain', () => {
    const s = type(mk(d, inner + 4), inner + 4, '\n', inner + 4, 'input');
    expect(s.doc.toString()).toBe('x =={coral}abcd==\n y');
  });

  it('pasting several lines inside highlights each line', () => {
    const s = type(mk(d, inner + 2), inner + 2, '1\n2\n3', inner + 2, 'input.paste');
    expect(s.doc.toString()).toBe('x =={coral}ab1==\n=={coral}2==\n=={coral}3cd== y');
  });

  it('Backspace just after a highlight removes the highlight, keeping its text', () => {
    const end = inner + 4 + 2; // after the closer
    const s = apply(mk(d, end), { changes: { from: end - 1, to: end }, selection: { anchor: end - 1 }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('x abcd y');
    expect(s.selection.main.head).toBe('x abcd'.length);
  });

  it('Backspace at the start of the text also unwraps it', () => {
    const s = apply(mk(d, inner), { changes: { from: inner - 1, to: inner }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('x abcd y');
    expect(s.selection.main.head).toBe(2);
  });

  it('deleting all of its text removes the delimiters too', () => {
    const s = apply(mk(d, inner, inner + 4), { changes: { from: inner, to: inner + 4 }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('x  y');
  });

  it('a deletion crossing into a highlight keeps the rest of it highlighted', () => {
    const s = apply(mk(d, 0, inner + 2), { changes: { from: 0, to: inner + 2 }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('=={coral}cd== y');
  });

  it('typing over a selection crossing out of a highlight keeps the rest of it', () => {
    const s = type(mk(d, inner + 2, d.length), inner + 2, 'Q', d.length);
    expect(s.doc.toString()).toBe('x =={coral}abQ==');
  });

  it('typing "==" inside splits it instead of breaking it', () => {
    let s = type(mk(d, inner + 2), inner + 2, '=');
    s = type(s, s.selection.main.head, '=');
    expect(stripHighlights(s.doc.toString())).toBe('x ab==cd y');
    expect(s.doc.toString()).toBe('x =={coral}ab======{coral}cd== y');
  });

  it('a whole highlight can be deleted', () => {
    const s = apply(mk(d, 2, d.length - 2), { changes: { from: 2, to: d.length - 2 }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('x  y');
  });

  it('typing just before or after a highlight stays outside it', () => {
    expect(type(mk(d, 2), 2, 'Q').doc.toString()).toBe('x Q=={coral}abcd== y');
    const end = d.indexOf('== y') + 2;
    expect(type(mk(d, end), end, 'Q').doc.toString()).toBe('x =={coral}abcd==Q y');
  });

  it('replace-all never edits a delimiter', () => {
    const doc = 'coral =={coral}coral==';
    const changes = [...doc.matchAll(/coral/g)].map((m) => ({ from: m.index!, to: m.index! + 5, insert: 'X' }));
    const s = apply(mk(doc), { changes, userEvent: 'input.replace.all' });
    expect(s.doc.toString()).toBe('X =={coral}X==');
  });

  it('the cursor never rests inside a delimiter', () => {
    const s = apply(mk(d, 0), { selection: { anchor: inner - 2 }, userEvent: 'select.pointer' });
    expect([2, inner]).toContain(s.selection.main.head);
  });

  it('programmatic edits are not reshaped', () => {
    const s = apply(mk(d), { changes: { from: inner - 1, to: inner } });
    expect(s.doc.toString()).toBe('x =={coralabcd== y');
  });
});

describe('found in review', () => {
  it('a cursor just outside a highlight does not target it', () => {
    const d = 'x =={coral}word== y';
    expect(hl(mk(d, 2), null).doc.toString()).toBe(d);
    expect(hl(mk(d, d.indexOf(' y')), null).doc.toString()).toBe(d);
  });

  it('a stray unclosed opener is never completed by a new highlight', () => {
    const d = 'literal =={coral}unterminated';
    const s = hl(mk(d, ...sel(d, 'terminated')), 'sky');
    expect(stripHighlights(s.doc.toString())).toBe(stripHighlights(d));
  });

  it('long lines work like any other', () => {
    const d = 'q'.repeat(12000) + ' keep tail';
    const s = hl(mk(d, ...sel(d, 'keep')), 'sky');
    expect(s.doc.toString().endsWith(' =={sky}keep== tail')).toBe(true);
    expect(stripHighlights(s.doc.toString())).toBe(d);
  });

  it('never splits an emoji', () => {
    const d = 'a😀b';
    const s = hl(mk(d, 1, 2), 'sky');
    expect(s.doc.toString()).toBe('a=={sky}😀==b');
  });

  it('"=" typed at the end of a highlight goes after it, in order', () => {
    const d = '=={amber}ab==';
    let s = type(mk(d, 11), 11, '=');
    s = type(s, s.selection.main.head, 'c');
    expect(s.doc.toString()).toBe('=={amber}ab===c');
    expect(stripHighlights(s.doc.toString())).toBe('ab=c');
  });

  it('typing over a backwards selection that crosses a delimiter keeps the typed order', () => {
    const d = 'p =={amber}cd==';
    let s = mk(d, d.indexOf('d'), 1); // head before the highlight
    s = apply(s, { changes: { from: 1, to: d.indexOf('d'), insert: 'Z' }, userEvent: 'input.type' });
    s = type(s, s.selection.main.head, 'W');
    expect(stripHighlights(s.doc.toString())).toBe('pZWd');
  });

  it('a highlight left with only spaces loses its delimiters', () => {
    const d = 'x =={coral}ab cd== y';
    const i = d.indexOf('ab cd');
    const s = apply(mk(d, i, i + 5), { changes: { from: i, to: i + 5, insert: ' ' }, selection: { anchor: i + 1 }, userEvent: 'input.type' });
    expect(s.doc.toString()).toBe('x   y');
  });

  it('an image dropped into a highlight closes and reopens it around the image', () => {
    const d = 'T\n=={coral}abcd==';
    const at = d.indexOf('cd');
    const s = apply(mk(d, at), { changes: { from: at, insert: IMG }, userEvent: 'input.drop' });
    expect(s.doc.toString()).toBe(`T\n=={coral}ab==\n${IMG}\n=={coral}cd==`);
  });
});

describe('copy and paste', () => {
  it('copying part of a highlight gives clean text, and balanced raw text for this app', () => {
    const d = 'x =={coral}hello world== after';
    const s = mk(d);
    const from = d.indexOf('world');
    expect(cleanSlice(s, from, d.length)).toEqual({ plain: 'world after', raw: '=={coral}world== after' });
    expect(cleanSlice(s, 0, d.indexOf(' world'))).toEqual({ plain: 'x hello', raw: 'x =={coral}hello==' });
  });

  it('the clipboard text never has half a delimiter', () => {
    const d = 'pre =={amber}abcd== post';
    const s = mk(d, 0, d.indexOf('cd'));
    const out = s.facet(EditorView.clipboardOutputFilter).reduce((t, f) => f(t, s), s.sliceDoc(0, d.indexOf('cd')));
    expect(out).toBe('pre ab');
  });

  const out = (s: EditorState) => s.facet(EditorView.clipboardOutputFilter);
  it('other apps get the text without delimiters', () => {
    const s = mk('a =={coral}b== c');
    expect(out(s).reduce((t, f) => f(t, s), 'a =={coral}b== c')).toBe('a b c');
  });
});
