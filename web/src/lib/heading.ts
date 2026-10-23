// Section headings stored in the note itself: "## {colour} Name" (SPEC.md §22). Pure text helpers.
// The server has the same grammar in internal/core/section.go; both are checked against
// internal/core/testdata/sections.json.
import { HUES } from './colors';

const NAMES = new Set(Object.keys(HUES).flatMap((h) => [h, `${h}-deep`]));
const HEAD_RE = /^## \{([a-z]+(?:-deep)?)\} (.*)$/;

export interface Heading {
  color: string;
  /** the name as written (may have leading spaces) */
  name: string;
  /** length of "## {colour} " */
  prefix: number;
}

/** The heading on this line, or null: a known colour and a name that isn't blank. */
export function parseHeading(text: string): Heading | null {
  if (!text.startsWith('## {')) return null;
  const m = HEAD_RE.exec(text);
  if (!m || !NAMES.has(m[1]) || !m[2].trim()) return null;
  return { color: m[1], name: m[2], prefix: m[1].length + 6 };
}

export const isHeadingLine = (text: string) => parseHeading(text) !== null;

export const headingLine = (color: string, name: string) => `## {${color}} ${name}`;

/** The text without section markup (names kept): what titles, counts and search see. */
export function stripSections(text: string): string {
  if (!text.includes('## {')) return text;
  return text
    .split('\n')
    .map((l) => parseHeading(l)?.name ?? l)
    .join('\n');
}

/** A slice [from, to) of one line, as other apps get it (a heading is just its name) and as this app
 * keeps it (raw). Null if the line isn't a heading. */
export function headingSlice(text: string, from: number, to: number): { plain: string; raw: string } | null {
  const h = text.startsWith('## {') ? parseHeading(text) : null;
  if (!h) return null;
  return { plain: text.slice(Math.max(from, h.prefix), Math.max(to, h.prefix)), raw: text.slice(from, to) };
}
