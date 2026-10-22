import { describe, expect, it } from 'vitest';
import { diffLines } from './diff';
import { MARKER_RE, markerLine, newRef, uuidv7 } from './ids';
import { refsIn } from './editor/markers';
import { bytes, fileKind } from './format';

describe('ids', () => {
  it('makes valid uuidv7s and refs', () => {
    const id = uuidv7();
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(newRef()).toMatch(/^[a-z2-7]{10}$/);
  });
  it('builds markers the server grammar accepts', () => {
    const line = markerLine('abcdefghij', 'my\nshot.png');
    expect(line).toBe('[image:abcdefghij] my shot.png');
    expect(MARKER_RE.test(line)).toBe(true);
    expect(refsIn(`a\n${line}\n [image:zzzzzzzzzz]`)).toEqual(new Set(['abcdefghij']));
  });
});

describe('diffLines', () => {
  it('marks added and removed lines', () => {
    const d = diffLines('a\nB\nc', 'a\nb\nc');
    expect(d.map((x) => x.kind)).toEqual(['same', 'theirs', 'mine', 'same']);
  });
});

describe('format', () => {
  it('formats sizes and kinds', () => {
    expect(bytes(512)).toBe('512 B');
    expect(bytes(1536)).toBe('1.5 KB');
    expect(fileKind('application/zip', 'x.zip').kind).toBe('archive');
  });
});
