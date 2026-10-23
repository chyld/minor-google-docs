import { describe, expect, it } from 'vitest';
import { EditorSelection, EditorState, type TransactionSpec } from '@codemirror/state';
import { history, undo } from '@codemirror/commands';
import { markers } from './markers';
import { imageEditing } from './images';
import { applyHighlight, cleanSlice, highlights } from './highlights';
import { rawEditing } from './raw';
import { foldedSections, insertSection, moveSection, recolorSection, removeSection, restoreFolds, sections, sectionsOf, setFolded } from './sections';

const IMG = '[image:aaaaaaaaaa] one.png';

function mk(doc: string, anchor = 0, head = anchor) {
  // same order as Editor.svelte
  return EditorState.create({ doc, selection: EditorSelection.single(anchor, head), extensions: [history(), markers(), sections(), highlights(), imageEditing()] });
}
const apply = (s: EditorState, spec: TransactionSpec) => s.update(spec).state;
const viewOf = (s: EditorState) => {
  const v = { state: s, dispatch: (tr: any) => (v.state = tr.state ?? v.state.update(tr).state) };
  return v as any;
};
const run = (s: EditorState, f: (v: any) => unknown) => {
  const v = viewOf(s);
  f(v);
  return v.state as EditorState;
};
const at = (doc: string, text: string, off = 0) => doc.indexOf(text) + off;
const type = (s: EditorState, from: number, insert: string, to = from, userEvent = 'input.type') =>
  apply(s, { changes: { from, to, insert }, selection: { anchor: from + insert.length }, userEvent });

const D = 'Title\nintro\n## {teal} Setup\nline one\n\n## {coral-deep} Logs\nlog line\n\n';

describe('finding sections', () => {
  it('lists headings after the title, with their extents', () => {
    const s = mk(D);
    const secs = sectionsOf(s);
    expect(secs.map((x) => [x.name, x.color])).toEqual([
      ['Setup', 'teal'],
      ['Logs', 'coral-deep'],
    ]);
    expect(s.sliceDoc(secs[0].from, secs[0].end)).toBe('## {teal} Setup\nline one\n');
    expect(s.sliceDoc(secs[0].headTo, secs[0].bodyEnd)).toBe('\nline one'); // trailing blank lines aren't body
    expect(s.sliceDoc(secs[1].from, secs[1].end)).toBe('## {coral-deep} Logs\nlog line\n\n');
  });

  it('the title line is never a section; plain ## lines are text', () => {
    const s = mk('## {teal} Title\n## Plain\n## {nope} x\n## {sky} Real');
    expect(sectionsOf(s).map((x) => x.name)).toEqual(['Real']);
  });
});

describe('adding, recolouring, removing', () => {
  it('starts a section after the cursor line, with its name selected', () => {
    const d = 'Title\nsome text\nmore';
    const s = run(mk(d, at(d, 'some')), (v) => insertSection(v, 'sky'));
    expect(s.doc.toString()).toBe('Title\nsome text\n## {sky} New section\nmore');
    const r = s.selection.main;
    expect(s.sliceDoc(r.from, r.to)).toBe('New section');
    const typed = apply(s, { changes: { from: r.from, to: r.to, insert: 'Network' }, selection: { anchor: r.from + 7 }, userEvent: 'input.type' });
    expect(sectionsOf(typed)[0].name).toBe('Network');
  });

  it('turns a blank line into the heading; never goes on or above the title', () => {
    const d = 'Title\n\nbody';
    expect(run(mk(d, at(d, '\n\n', 1)), (v) => insertSection(v, 'mint')).doc.toString()).toBe('Title\n## {mint} New section\nbody');
    expect(run(mk(d, 2), (v) => insertSection(v, 'mint')).doc.toString()).toBe('Title\n## {mint} New section\n\nbody');
  });

  it('goes after a selected image', () => {
    const d = `Title\n${IMG}\nafter`;
    const s = run(mk(d, at(d, IMG), at(d, IMG) + IMG.length), (v) => insertSection(v, 'teal'));
    expect(s.doc.toString()).toBe(`Title\n${IMG}\n## {teal} New section\nafter`);
  });

  it('recolours and removes in one undo step each', () => {
    let s = run(mk(D), (v) => recolorSection(v, 0, 'violet'));
    expect(s.doc.toString()).toContain('## {violet} Setup');
    s = run(s, (v) => removeSection(v, 1));
    expect(s.doc.toString()).toContain('\nLogs\nlog line');
    let back = s;
    undo({ state: s, dispatch: (tr) => (back = tr.state) });
    expect(back.doc.toString()).toContain('## {coral-deep} Logs');
  });
});

describe('edits keep headings whole', () => {
  const d = 'Title\n## {teal} Setup\nbody';
  const nameAt = at(d, 'Setup');

  it('Backspace at the name start (hidden: the whole prefix) turns it into text', () => {
    const s = apply(mk(d, nameAt), { changes: { from: at(d, '## '), to: nameAt }, selection: { anchor: at(d, '## ') }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('Title\nSetup\nbody');
  });

  it('…and so does a one-character Backspace when the prefix is showing', () => {
    const s = apply(mk(d, nameAt), { changes: { from: nameAt - 1, to: nameAt }, selection: { anchor: nameAt - 1 }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('Title\nSetup\nbody');
  });

  it('typing inside the prefix lands at the name start', () => {
    const s = type(mk(d, nameAt), nameAt - 3, 'X');
    expect(s.doc.toString()).toBe('Title\n## {teal} XSetup\nbody');
  });

  it('Enter at the name start opens a line above the heading', () => {
    const s = type(mk(d, nameAt), nameAt, '\n', nameAt, 'input');
    expect(s.doc.toString()).toBe('Title\n\n## {teal} Setup\nbody');
    expect(s.selection.main.head).toBe(nameAt + 1);
  });

  it('Enter at the end starts the body; in the middle, the rest goes to the body', () => {
    const end = at(d, 'Setup') + 5;
    expect(type(mk(d, end), end, '\n', end, 'input').doc.toString()).toBe('Title\n## {teal} Setup\n\nbody');
    const mid = at(d, 'Setup') + 3;
    const s = type(mk(d, mid), mid, '\n', mid, 'input');
    expect(s.doc.toString()).toBe('Title\n## {teal} Set\nup\nbody');
    expect(sectionsOf(s)[0].name).toBe('Set');
  });

  it('Backspace at the first body line start moves up instead of merging', () => {
    const b = at(d, 'body');
    const s = apply(mk(d, b), { changes: { from: b - 1, to: b }, selection: { anchor: b - 1 }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe(d);
    expect(s.selection.main.head).toBe(b - 1);
  });

  it('Delete at the heading end moves down instead of merging', () => {
    const e = at(d, 'Setup') + 5;
    const s = apply(mk(d, e), { changes: { from: e, to: e + 1 }, selection: { anchor: e }, userEvent: 'delete.forward' });
    expect(s.doc.toString()).toBe(d);
    expect(s.selection.main.head).toBe(e + 1);
  });

  it('Delete at the end of the line above doesn\'t pull the heading up', () => {
    const e = at(d, 'Title') + 5;
    const s = apply(mk(d, e), { changes: { from: e, to: e + 1 }, selection: { anchor: e }, userEvent: 'delete.forward' });
    expect(s.doc.toString()).toBe(d);
  });

  it('deleting the whole name leaves no stray prefix', () => {
    const s = apply(mk(d, nameAt, nameAt + 5), { changes: { from: nameAt, to: nameAt + 5 }, selection: { anchor: nameAt }, userEvent: 'delete.backward' });
    expect(s.doc.toString()).toBe('Title\n\nbody');
  });

  it('the cursor never rests inside the prefix', () => {
    const s = apply(mk(d, 0), { selection: { anchor: at(d, '## ') + 2 }, userEvent: 'select' });
    expect(s.selection.main.head).toBe(nameAt);
    const left = apply(mk(d, nameAt), { selection: { anchor: at(d, '## ') }, userEvent: 'select' });
    expect(left.selection.main.head).toBe(at(d, '## ') - 1); // leaving leftwards: end of the line above
  });

  it('replace-all never rewrites the prefix (rendered and raw)', () => {
    const s = apply(mk(d), { changes: [{ from: at(d, 'teal'), to: at(d, 'teal') + 4, insert: 'sky' }], userEvent: 'input.replace' });
    expect(s.doc.toString()).toBe(d);
    const raw = EditorState.create({ doc: d, extensions: [rawEditing()] }).update({ changes: [{ from: at(d, 'teal'), to: at(d, 'teal') + 4, insert: 'sky' }], userEvent: 'input.replace' }).state;
    expect(raw.doc.toString()).toBe(d);
  });

  it('highlights never go into a heading', () => {
    const s = run(mk(d, at(d, 'Title'), at(d, 'body') + 4), (v) => applyHighlight(v, 'amber'));
    expect(s.doc.toString()).toBe('=={amber}Title==\n## {teal} Setup\n=={amber}body==');
  });

  it('copy: other apps get the name, this app keeps the heading', () => {
    const s = mk(d);
    expect(cleanSlice(s, 0, d.length)).toEqual({ plain: 'Title\nSetup\nbody', raw: d });
    expect(cleanSlice(s, nameAt + 1, nameAt + 4)).toEqual({ plain: 'etu', raw: 'etu' });
  });
});

describe('moving and collapsing', () => {
  it('swaps a section with its neighbour; the text before the first section stays', () => {
    let s = run(mk(D, at(D, 'log line')), (v) => moveSection(v, 1, -1));
    expect(s.doc.toString()).toBe('Title\nintro\n## {coral-deep} Logs\nlog line\n\n\n## {teal} Setup\nline one\n');
    expect(s.sliceDoc(s.selection.main.head, s.selection.main.head + 8)).toBe('log line'); // the cursor travels
    s = run(s, (v) => moveSection(v, 0, 1));
    expect(s.doc.toString()).toBe('Title\nintro\n## {teal} Setup\nline one\n\n## {coral-deep} Logs\nlog line\n\n');
    expect(run(s, (v) => moveSection(v, 0, -1)).doc.toString()).toBe(s.doc.toString()); // nothing above the first
  });

  it('moving is one undo step', () => {
    const s = run(mk(D), (v) => moveSection(v, 0, 1));
    let back = s;
    undo({ state: s, dispatch: (tr) => (back = tr.state) });
    expect(back.doc.toString()).toBe(D);
  });

  it('collapses, remembers, and a moved collapsed section stays collapsed', () => {
    let s = run(mk(D), (v) => setFolded(v, 0, true));
    expect(foldedSections(s)).toEqual([0]);
    s = run(s, (v) => moveSection(v, 0, 1));
    expect(foldedSections(s)).toEqual([1]);
    const fresh = run(mk(D), (v) => restoreFolds(v, [1]));
    expect(foldedSections(fresh)).toEqual([1]);
  });
});

describe('found in review', () => {
  it('a cursor in the other section travels with it when moving', () => {
    // cursor in Logs (index 1); move Setup (index 0) down past it
    const s = run(mk(D, at(D, 'log line') + 3), (v) => moveSection(v, 0, 1));
    expect(s.sliceDoc(s.selection.main.head - 3, s.selection.main.head + 5)).toBe('log line');
  });

  it('removing a collapsed section shows its text again', () => {
    let s = run(mk(D), (v) => setFolded(v, 0, true));
    s = run(s, (v) => removeSection(v, 0));
    expect(foldedSections(s)).toEqual([]);
    expect(s.doc.toString()).toContain('\nSetup\nline one');
  });

  it('a selection reaching the end of a collapsed body opens it', () => {
    let s = run(mk(D), (v) => setFolded(v, 0, true));
    const a = at(D, 'line one');
    s = apply(s, { selection: { anchor: a, head: a + 8 }, userEvent: 'select.search' });
    expect(foldedSections(s)).toEqual([]);
  });

  it('a cursor at the heading end leaves it collapsed', () => {
    let s = run(mk(D), (v) => setFolded(v, 0, true));
    s = apply(s, { selection: { anchor: at(D, 'Setup') + 5 }, userEvent: 'select' });
    expect(foldedSections(s)).toEqual([0]);
  });
});
