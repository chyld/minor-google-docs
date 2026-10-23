import { describe, expect, it } from 'vitest';
import { EditorSelection, EditorState, type TransactionSpec } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { history, undo } from '@codemirror/commands';
import { markers } from './markers';
import { imageEditing, initialSelection, selectedImage } from './images';

const A = '[image:aaaaaaaaaa] one.png';
const B = '[image:bbbbbbbbbb] two.png';

function mk(doc: string, anchor = 0, head = anchor) {
  return EditorState.create({ doc, selection: EditorSelection.single(anchor, head), extensions: [history(), markers(), imageEditing()] });
}
const apply = (s: EditorState, spec: TransactionSpec) => s.update(spec).state;
const lineOf = (s: EditorState, n: number) => s.doc.line(n);

describe('image lines change only as whole lines', () => {
  const doc = `Title\n${A}\nafter`;

  it('Backspace at the start of the line after an image does not glue it into the caption', () => {
    const s = mk(doc);
    const a = lineOf(s, 3).from;
    const next = apply(s, { changes: { from: a - 1, to: a }, userEvent: 'delete.backward' });
    expect(next.doc.toString()).toBe(doc);
  });

  it('Delete at the end of the line before an image does not turn it into raw text', () => {
    const s = mk(doc);
    const t = lineOf(s, 1).to;
    expect(apply(s, { changes: { from: t, to: t + 1 }, userEvent: 'delete.forward' }).doc.toString()).toBe(doc);
  });

  it('two consecutive images cannot be merged', () => {
    const d = `T\n${A}\n${B}\nx`;
    const s = mk(d);
    const t = lineOf(s, 2).to;
    expect(apply(s, { changes: { from: t, to: t + 1 }, userEvent: 'delete.forward' }).doc.toString()).toBe(d);
  });

  it('an empty line next to an image can be removed, and the image becomes selected', () => {
    const s = mk(`Title\n${A}\n\nafter`);
    const blank = lineOf(s, 3);
    const next = apply(s, { changes: { from: blank.from - 1, to: blank.from }, selection: { anchor: blank.from - 1 }, userEvent: 'delete.backward' });
    expect(next.doc.toString()).toBe(`Title\n${A}\nafter`);
    expect(selectedImage(next)?.text).toBe(A);
  });

  it('typing over a selected image puts the text on a new line after it', () => {
    const s0 = mk(doc);
    const img = lineOf(s0, 2);
    const s = mk(doc, img.from, img.to);
    const next = apply(s, { changes: { from: img.from, to: img.to, insert: 'x' }, userEvent: 'input.type' });
    expect(next.doc.toString()).toBe(`Title\n${A}\nx\nafter`);
    expect(next.selection.main.head).toBe(img.to + 2);
  });

  it('cutting exactly the image removes its line without leaving a blank one', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    const next = apply(s, { changes: { from: img.from, to: img.to }, userEvent: 'delete.cut' });
    expect(next.doc.toString()).toBe('Title\nafter');
  });

  it('a selection spanning text and images can be deleted', () => {
    const s = mk(doc, 0, doc.length);
    expect(apply(s, { changes: { from: 0, to: doc.length }, userEvent: 'delete.backward' }).doc.toString()).toBe('');
  });

  it('indenting an image line is refused', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    expect(apply(s, { changes: { from: img.from, insert: '\t' }, userEvent: 'input.indent' }).doc.toString()).toBe(doc);
  });

  it('typing over a selected line that ends at an image keeps the text on its own line', () => {
    const d = `Title\nreplace me\n${A}\nafter`;
    const s = mk(d);
    const from = lineOf(s, 2).from;
    const to = lineOf(s, 3).from; // triple-click selects the line and its newline
    const next = apply(s, { changes: { from, to, insert: 'X' }, selection: { anchor: from + 1 }, userEvent: 'input.type' });
    expect(next.doc.toString()).toBe(`Title\nX\n${A}\nafter`);
    expect(next.selection.main.head).toBe(from + 1);
  });

  it('pasting over text that starts right after an image keeps the image whole', () => {
    const d = `Title\n${A}\nold text`;
    const s = mk(d);
    const img = lineOf(s, 2);
    const next = apply(s, { changes: { from: img.to, to: d.length, insert: 'new' }, userEvent: 'input.paste' });
    expect(next.doc.toString()).toBe(`Title\n${A}\nnew`);
  });

  it('typing over an image plus its line break (Shift+arrow, select-line) keeps the image and the next line', () => {
    const d = `Title\nkeep-me\n${A}\nafter line`;
    const s = mk(d);
    const img = lineOf(s, 3);
    const fwd = apply(s, { changes: { from: img.from, to: img.to + 1, insert: 'x' }, userEvent: 'input.type' });
    expect(fwd.doc.toString()).toBe(`Title\nkeep-me\n${A}\nx\nafter line`);
    const back = apply(s, { changes: { from: img.from - 1, to: img.to, insert: 'y' }, userEvent: 'input.type' });
    expect(back.doc.toString()).toBe(`Title\nkeep-me\n${A}\ny\nafter line`);
  });

  it('deleting an image with both its line breaks does not merge the lines around it', () => {
    const d = `a\n${A}\nb`;
    const s = mk(d);
    const img = lineOf(s, 2);
    expect(apply(s, { changes: { from: img.from - 1, to: img.to + 1 }, userEvent: 'delete' }).doc.toString()).toBe('a\nb');
  });

  it('a new line can be inserted right after an image', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    expect(apply(s, { changes: { from: img.to, insert: '\nnew' }, userEvent: 'input' }).doc.toString()).toBe(`Title\n${A}\nnew\nafter`);
  });

  it('replace-all edits text but never image lines', () => {
    const d = `one.png notes\n${A}\nmore one.png`;
    const s = mk(d);
    const changes: { from: number; to: number; insert: string }[] = [];
    for (const m of d.matchAll(/one\.png/g)) changes.push({ from: m.index!, to: m.index! + 7, insert: 'X' });
    const next = apply(s, { changes, userEvent: 'input.replace.all' });
    expect(next.doc.toString()).toBe(`X notes\n${A}\nmore X`);
  });

  it('programmatic edits are not policed', () => {
    const s = mk(doc);
    const t = lineOf(s, 1).to;
    expect(apply(s, { changes: { from: t, to: t + 1 } }).doc.toString()).toBe(`Title${A}\nafter`);
  });
});

describe('the cursor stays off image lines', () => {
  const doc = `Title\n${A}\nafter`;

  it('a click landing on an image selects it', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    const next = apply(s, { selection: { anchor: img.to }, userEvent: 'select.pointer' });
    expect(selectedImage(next)?.from).toBe(img.from);
  });

  it('a programmatic cursor on an image moves to the next text line', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    const next = apply(s, { selection: { anchor: img.from } });
    expect(next.selection.main.head).toBe(lineOf(s, 3).from);
  });

  it('a range ending inside an image line grows to the whole image', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    const next = apply(s, { selection: { anchor: img.from + 20, head: img.from + 23 }, userEvent: 'select' });
    expect(selectedImage(next)?.from).toBe(img.from);
  });

  it('a search match inside a caption stays a match (so Replace can move past it)', () => {
    const s = mk(doc);
    const img = lineOf(s, 2);
    const next = apply(s, { selection: { anchor: img.from + 19, head: img.from + 26 }, userEvent: 'select.search' });
    expect([next.selection.main.from, next.selection.main.to]).toEqual([img.from + 19, img.from + 26]);
  });

  it('a note that starts with an image opens with the cursor on the first text line', () => {
    const s = EditorState.create({ doc: `${A}\n${B}\nhello` });
    expect(initialSelection(s.doc).main.head).toBe(s.doc.line(3).from);
  });

  it('undo of a guarded edit restores the image in one step', () => {
    let s = mk(doc);
    const img = lineOf(s, 2);
    s = apply(s, { selection: { anchor: img.from, head: img.to }, userEvent: 'select' });
    s = apply(s, { changes: { from: img.from, to: img.to }, userEvent: 'delete.cut' });
    expect(s.doc.toString()).toBe('Title\nafter');
    let restored = s;
    undo({ state: s, dispatch: (tr) => (restored = tr.state) });
    expect(restored.doc.toString()).toBe(doc);
  });
});

describe('dropped image lines get their own line', () => {
  it('marker text dropped mid-line is split out', () => {
    const s = mk('target text here', 0);
    const next = apply(s, { changes: { from: 7, insert: A }, userEvent: 'input.drop' });
    expect(next.doc.toString()).toBe(`target \n${A}\ntext here`);
  });
});

describe('pasted image lines get their own line', () => {
  const filters = (s: EditorState) => s.facet(EditorView.clipboardInputFilter);
  const paste = (s: EditorState, text: string) => filters(s).reduce((t, f) => f(t, s), text);

  it('mid-line paste of a copied image', () => {
    const s = mk('foobar', 3);
    expect(paste(s, A)).toBe(`\n${A}\n`);
  });

  it('plain text is untouched', () => {
    const s = mk('foobar', 3);
    expect(paste(s, 'hello')).toBe('hello');
  });
});
