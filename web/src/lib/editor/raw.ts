import { EditorState, type Extension } from '@codemirror/state';
import { EditorView } from '@codemirror/view';
import { parseLine } from '../highlight';
import { MARKER_RE } from '../ids';
import { RAW_MIME } from './highlights';

/**
 * Raw text mode (SPEC.md §20): the stored text with no rendering and no edit rules, except two
 * safety nets. Search-and-replace never rewrites an image's `[image:ref]` or a highlight's delimiters
 * (a "replace all" would silently cut images loose or recolour text), and text copied in the rendered
 * view pastes with its highlights.
 */
const protectOnReplace = EditorState.changeFilter.of((tr) => {
  if (!tr.isUserEvent('input.replace')) return true;
  const out: number[] = [];
  const doc = tr.startState.doc;
  for (let n = 1; n <= doc.lines; n++) {
    const l = doc.line(n);
    const m = MARKER_RE.exec(l.text);
    if (m) out.push(l.from, l.from + m[1].length + 8); // "[image:" + ref + "]"
    else for (const h of parseLine(l.text, l.from)) out.push(h.from, h.innerFrom, h.innerTo, h.to);
  }
  return out.length ? out : true;
});

const pasteRaw = EditorView.domEventHandlers({
  paste(e, view) {
    const data = e.clipboardData;
    const raw = data?.getData(RAW_MIME);
    if (!raw || data!.files.length || view.state.readOnly) return false; // files are the editor's own handler's
    e.preventDefault();
    view.dispatch(view.state.replaceSelection(raw), { userEvent: 'input.paste', scrollIntoView: true });
    return true;
  },
});

export function rawEditing(): Extension {
  return [protectOnReplace, pasteRaw];
}
