import { describe, expect, it } from 'vitest';
import { EditorState } from '@codemirror/state';
import { rawEditing } from './raw';

const mk = (doc: string) => EditorState.create({ doc, extensions: [rawEditing()] });
/** Replace every occurrence of `find` as search-and-replace would. */
function replaceAll(s: EditorState, find: string, insert: string, userEvent = 'input.replace') {
  const doc = s.doc.toString();
  const changes = [];
  for (let i = doc.indexOf(find); i >= 0; i = doc.indexOf(find, i + find.length)) changes.push({ from: i, to: i + find.length, insert });
  return s.update({ changes, userEvent }).state.doc.toString();
}

describe('raw text mode', () => {
  it('replace-all leaves image refs and highlight delimiters alone', () => {
    const d = 'image notes\n[image:abcdefghij] image.png\nan =={amber}amber image== here';
    expect(replaceAll(mk(d), 'image', 'pic')).toBe('pic notes\n[image:abcdefghij] pic.png\nan =={amber}amber pic== here');
    expect(replaceAll(mk(d), 'amber', 'teal')).toBe('image notes\n[image:abcdefghij] image.png\nan =={amber}teal image== here');
  });

  it('typing and deleting are not filtered', () => {
    const d = 'x =={amber}y== z\n[image:abcdefghij] a.png';
    expect(replaceAll(mk(d), '==', '', 'input.type')).toBe('x {amber}y z\n[image:abcdefghij] a.png');
    expect(replaceAll(mk(d), '[image:', '[img:', 'delete')).toBe('x =={amber}y== z\n[img:abcdefghij] a.png');
  });
});
