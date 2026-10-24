<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { EditorState, Annotation, Compartment, EditorSelection, Prec, Transaction, type Line } from '@codemirror/state';
  import { EditorView, drawSelection, keymap, placeholder } from '@codemirror/view';
  import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
  import { search, searchKeymap, searchPanelOpen } from '@codemirror/search';
  import { markers } from '../lib/editor/markers';
  import { rawEditing } from '../lib/editor/raw';
  import { foldEffect, unfoldEffect } from '@codemirror/language';
  import { type SectionInfo, foldedSections, gotoSection, insertSection, isFolded, moveSection, recolorSection, removeSection, restoreFolds, sectionAt, sections, sectionsOf, setFolded } from '../lib/editor/sections';
  import { isHeadingLine } from '../lib/heading';
  import { imageEditing, imageLineRange, imageOp, initialSelection, isImageLine, selectedImage, textPos } from '../lib/editor/images';
  import { applyHighlight, applyStyle, highlightAt, highlights, styleState, type HighlightInfo, type StyleOp } from '../lib/editor/highlights';
  import { stamp } from '../lib/format';
  import { MARKER_RE, markerLine } from '../lib/ids';

  interface Props {
    readonly?: boolean;
    raw?: boolean; // show and edit the stored text as-is: no images, colours or title styling
    onchange: (text: string) => void;
    onfiles: (files: File[], at: 'cursor' | number) => void;
    onsave: () => void;
    onblur: () => void;
    onselection?: (info: HighlightInfo) => void;
    onhighlightkey?: () => void;
    /** the note's sections changed (added, renamed, recoloured, moved, collapsed) */
    onsections?: (list: SectionInfo[]) => void;
  }

  let { readonly = false, raw = false, onchange, onfiles, onsave, onblur, onselection, onhighlightkey, onsections }: Props = $props();
  let dragging = false;

  let host: HTMLDivElement;
  let view: EditorView | null = null;
  const external = Annotation.define<boolean>();
  // a selection re-checked by the rendered view's rules (not the user placing the cursor)
  const recheck = Annotation.define<boolean>();
  const rich = new Compartment(); // what turns the stored text into the rendered note
  const richExt = [
    markers(),
    // filters run last-registered first: image and highlight rules shape an edit before the section rules see it
    sections(),
    // highlights first: transaction filters run last-registered first, so the image rules shape an
    // edit (e.g. a dropped image gets its own line) before highlights split around the result
    highlights(),
    imageEditing(),
  ];
  const rawExt = rawEditing();
  const richExtensions = () => (untrack(() => raw) ? rawExt : richExt);
  // whether the user has placed the cursor since this note loaded (attachments go there, else at the end)
  let touched = false;
  // Refs whose upload was canceled (null) or re-minted (new ref). Undo/redo can bring their old
  // markers back from history; those are fixed up again right after.
  const retired = new Map<string, string | null>();

  function reapplyRetired() {
    if (!view || !retired.size) return;
    const doc = view.state.doc;
    for (let i = 1; i <= doc.lines; i++) {
      const ref = MARKER_RE.exec(doc.line(i).text)?.[1];
      if (!ref || !retired.has(ref)) continue;
      const next = retired.get(ref);
      if (next) replaceRef(ref, next);
      else removeMarker(ref);
      return reapplyRetired(); // positions changed; rescan
    }
  }

  function extensions(ro: boolean) {
    return [
      history(),
      search({ top: true }),
      Prec.highest(
        keymap.of([
          { key: 'Mod-Shift-h', preventDefault: true, run: () => (onhighlightkey?.(), true) },
          // bold / underline (SPEC §19): always swallowed in the editor, so the browser's own
          // Ctrl+B (bookmarks) / Ctrl+U (view source) never fire; nothing to do in raw or read-only
          { key: 'Mod-b', preventDefault: true, run: (v) => (v.composing ? false : (formatKey(v, 'b'), true)) },
          { key: 'Mod-u', preventDefault: true, run: (v) => (v.composing ? false : (formatKey(v, 'u'), true)) },
        ]),
      ),
      rich.of(richExtensions()),
      drawSelection({ drawRangeCursor: false }),
      EditorView.lineWrapping,
      placeholder('Start with a title…'),
      EditorState.readOnly.of(ro),
      EditorView.editable.of(!ro),
      keymap.of([
        { key: 'Mod-s', preventDefault: true, run: () => (onsave(), true) },
        {
          key: 'Alt-t',
          run: (v) => {
            v.dispatch(v.state.replaceSelection(stamp()), { userEvent: 'input' });
            return true;
          },
        },
        ...defaultKeymap,
        ...historyKeymap,
        ...searchKeymap,
        indentWithTab,
      ]),
      EditorView.updateListener.of((u) => {
        if (u.docChanged && !u.transactions.some((t) => t.annotation(external))) onchange(u.state.doc.toString());
        if (u.transactions.some((t) => t.annotation(Transaction.userEvent) && !t.annotation(recheck))) touched = true;
        if (u.docChanged && u.transactions.some((t) => t.isUserEvent('undo') || t.isUserEvent('redo'))) {
          queueMicrotask(reapplyRetired);
          queueMicrotask(recheckSelection); // history restores cursors without the edit rules
        }
        if (u.selectionSet || u.docChanged || u.focusChanged || u.geometryChanged) report();
        if (u.docChanged || u.selectionSet || u.startState.facet(EditorState.readOnly) !== u.state.facet(EditorState.readOnly) || u.transactions.some((t) => t.reconfigured || t.effects.some((e) => e.is(foldEffect) || e.is(unfoldEffect)))) reportSections();
      }),
      EditorView.domEventHandlers({
        paste(e) {
          const files = [...(e.clipboardData?.files ?? [])];
          if (!files.length) return false;
          e.preventDefault();
          onfiles(files, 'cursor');
          return true;
        },
        drop(e, v) {
          const files = [...(e.dataTransfer?.files ?? [])];
          if (!files.length) return false;
          e.preventDefault();
          const pos = v.posAtCoords({ x: e.clientX, y: e.clientY });
          onfiles(files, pos ?? 'cursor');
          return true;
        },
        blur() {
          onblur();
          return false;
        },
        mousedown(e) {
          if (e.button !== 0) return false;
          dragging = true;
          report();
          const up = () => {
            window.removeEventListener('mouseup', up, true);
            dragging = false;
            requestAnimationFrame(report); // after the click's selection lands
          };
          window.addEventListener('mouseup', up, true);
          return false;
        },
      }),
    ];
  }

  onMount(() => {
    view = new EditorView({ parent: host, state: EditorState.create({ doc: '', extensions: extensions(readonly) }) });
    return () => view?.destroy();
  });

  /** Run the rendered view's selection rules on the current selection (off image text, out of
   * hidden delimiters). */
  function recheckSelection() {
    if (!view || untrack(() => raw)) return;
    // a Find match keeps its exact range (as when Find selected it), or Backspace would act on the
    // whole image line / highlight
    const st = view.state;
    const found = searchPanelOpen(st) && !st.selection.main.empty;
    view.dispatch({ selection: st.selection, userEvent: found ? 'select.search' : 'select', annotations: recheck.of(true) });
  }

  // switching keeps the text, the cursor and undo history; only how the text is drawn changes
  $effect(() => {
    const r = raw;
    if (!view || rich.get(view.state) === (r ? rawExt : richExt)) return;
    restoring = true; // the reconfigure drops collapsed sections; they come back below, not forgotten
    view.dispatch({ effects: [rich.reconfigure(richExtensions()), EditorView.announce.of(r ? 'Raw text' : 'Rendered note')] });
    restoring = false;
    if (!r) restoreSavedFolds();
    reportSections();
    recheckSelection();
    report();
  });

  /** Replace the whole document without triggering onchange (loading a note). */
  export function setDoc(text: string, ro = false, folds: string | null = null) {
    if (!view) return;
    foldsKey = folds;
    touched = false;
    retired.clear(); // a fresh state has no history that could bring old markers back
    const doc = EditorState.create({ doc: text }).doc;
    const selection = untrack(() => raw) ? EditorSelection.single(0) : initialSelection(doc);
    view.setState(EditorState.create({ doc, selection, extensions: extensions(ro) }));
    lastSections = '';
    restoreSavedFolds();
    reportSections();
  }

  function formatKey(v: EditorView, style: 'b' | 'u') {
    if (untrack(() => raw) || v.state.readOnly || selectedImage(v.state)) return;
    applyStyle(v, { kind: 'toggle', style });
  }

  // the bold/underline state is exact for selections up to this size (computing it for a whole
  // large note on every cursor move would be slow); beyond it the buttons show "some"
  const STATE_LIMIT = 50_000;

  function highlightInfo(): HighlightInfo {
    const none: HighlightInfo = { can: false, empty: true, current: null, focused: false, dragging, rect: null, bold: 'none', underline: 'none', hasColor: false, styled: false };
    if (!view) return none;
    const st = view.state;
    const focused = view.hasFocus;
    if (st.readOnly || raw || selectedImage(st)) return { ...none, focused };
    const r = st.selection.main;
    for (let pos = r.from; pos <= r.to; ) {
      const l = st.doc.lineAt(pos);
      if (isHeadingLine(l.text)) return { ...none, focused }; // section headings take no highlights
      pos = l.to + 1;
    }
    const h = highlightAt(st);
    const big = r.to - r.from > STATE_LIMIT;
    const ss = big ? { bold: 'some' as const, underline: 'some' as const, hasColor: true, styled: true, color: null } : styleState(st);
    const styles = { bold: ss.bold, underline: ss.underline, hasColor: ss.hasColor, styled: ss.styled, current: ss.color };
    if (r.empty) return { ...none, ...styles, can: !!h, focused };
    const c = view.coordsAtPos(r.from, 1);
    return { ...styles, can: true, empty: false, focused, dragging, rect: c ? { left: c.left, top: c.top, bottom: c.bottom } : null };
  }

  // ---- sections ----

  // collapsed sections are remembered per note in this browser (never in the file), under foldsKey
  let foldsKey: string | null = null;
  let restoring = false;
  let lastSections = '';

  function restoreSavedFolds() {
    if (!view || !foldsKey || untrack(() => raw)) return;
    let saved: number[] = [];
    try {
      const v = JSON.parse(localStorage.getItem(foldsKey) ?? '[]');
      if (Array.isArray(v)) saved = v.filter((x) => Number.isInteger(x));
    } catch {
      /* ignore a bad entry */
    }
    restoring = true;
    restoreFolds(view, saved);
    restoring = false;
  }

  function reportSections() {
    if (!view || restoring) return;
    const st = view.state;
    const isRaw = untrack(() => raw);
    const list: SectionInfo[] = isRaw ? [] : sectionsOf(st).map((s) => ({ name: s.name, color: s.color, folded: isFolded(st, s), foldable: s.bodyEnd > s.headTo }));
    const key = JSON.stringify(list);
    if (key === lastSections) return;
    lastSections = key;
    onsections?.(list);
    if (isRaw || !foldsKey || st.readOnly) return;
    const folded = foldedSections(st);
    if (folded.length) localStorage.setItem(foldsKey, JSON.stringify(folded));
    else localStorage.removeItem(foldsKey);
  }

  /** Start a section after the cursor's line, in this colour (the name is selected for typing). */
  export function addSection(color: string): boolean {
    if (!view || untrack(() => raw)) return false;
    const done = insertSection(view, color);
    view.focus();
    return done;
  }
  export function sectionColor(index: number, color: string) {
    if (view) recolorSection(view, index, color);
    view?.focus();
  }
  export function sectionRemove(index: number) {
    if (view) removeSection(view, index);
    view?.focus();
  }
  export function sectionMove(index: number, dir: -1 | 1) {
    if (view) moveSection(view, index, dir);
    view?.focus();
  }
  export function sectionGo(index: number) {
    if (view) gotoSection(view, index);
  }
  export function sectionFold(index: number, fold: boolean) {
    if (view) setFolded(view, index, fold);
  }
  /** Open the menu of the section the cursor is in (keyboard access); false if it's in none. */
  export function openSectionMenu(): boolean {
    if (!view || untrack(() => raw)) return false;
    const s = sectionAt(view.state, view.state.selection.main.head);
    const c = s && view.coordsAtPos(s.headTo);
    if (!s || !c) return false;
    view.dom.dispatchEvent(new CustomEvent('notes:section-menu', { bubbles: true, detail: { index: s.index, rect: { left: c.left, top: c.top, bottom: c.bottom, right: c.right } } }));
    return true;
  }

  /** Tell the parent about the selection again (e.g. after its scroll container moved). */
  export function report() {
    onselection?.(highlightInfo());
  }

  /** Colour the selection (null removes the colour; bold and underline stay), then return to typing. */
  export function highlight(name: string | null): boolean {
    if (!view) return false;
    const done = applyHighlight(view, name);
    view.focus();
    return done;
  }

  /** Toggle bold/underline or clear all formatting on the selection, then return to typing. */
  export function format(op: StyleOp, focus = true): boolean {
    if (!view || untrack(() => raw) || view.state.readOnly) return false;
    const done = applyStyle(view, op);
    if (focus) view.focus();
    report();
    return done;
  }

  /** Replace content as an edit (undoable, triggers onchange). */
  export function replaceAll(text: string) {
    view?.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
  }

  export function getDoc(): string {
    return view?.state.doc.toString() ?? '';
  }

  /** Cursor at the end of the note (or the last image selected, if the note ends with one). */
  function endSelection(st: EditorState): EditorSelection {
    const last = st.doc.line(st.doc.lines);
    return isImageLine(last) && !raw ? EditorSelection.single(last.from, last.to) : EditorSelection.single(st.doc.length);
  }

  export function focus(atEnd = false) {
    if (!view) return;
    view.focus();
    if (atEnd) view.dispatch({ selection: endSelection(view.state), annotations: imageOp.of(true) });
  }

  /** Extend the selection to the end of the note (Shift+click under it). */
  export function extendToEnd() {
    if (!view) return;
    view.focus();
    const r = view.state.selection.main;
    view.dispatch({ selection: { anchor: r.anchor, head: view.state.doc.length }, userEvent: 'select', scrollIntoView: true });
  }

  /** Focus for adding attachments: keep the user's cursor if they placed one, else go to the end. */
  export function focusForInsert() {
    if (!view) return;
    view.focus();
    if (!touched) view.dispatch({ selection: endSelection(view.state), annotations: imageOp.of(true) });
  }

  /** The line new images go after (or, if empty, in place of): never above the title line. */
  function insertionLine(st: EditorState, at: 'cursor' | number): Line {
    const doc = st.doc;
    let line: Line;
    if (at !== 'cursor') line = doc.lineAt(at);
    else {
      const r = st.selection.main;
      line = selectedImage(st) ?? doc.lineAt(r.empty ? r.head : r.to);
    }
    for (let n = 1; n < line.number; n++) {
      const l = doc.line(n);
      if (l.text.trim() !== '' && !isImageLine(l)) return line; // the title is above: fine
    }
    // nothing but blank/image lines above: if a title exists below, go after it
    for (let n = line.number; n <= doc.lines; n++) {
      const l = doc.line(n);
      if (l.text.trim() !== '' && !isImageLine(l)) return n === line.number ? line : l;
    }
    return line;
  }

  /** Insert marker lines for images on their own lines, then leave the cursor on a text line below them. */
  export function insertMarkers(items: { ref: string; filename: string }[], at: 'cursor' | number) {
    if (!view || !items.length) return;
    const st = view.state;
    const doc = st.doc;
    const line = insertionLine(st, at);
    const block = items.map((i) => markerLine(i.ref, i.filename)).join('\n');
    let from: number, insert: string, cursor: number;
    if (!isImageLine(line) && line.text.trim() === '') {
      // a blank line: images go here and the blank line stays below them for typing
      from = line.from;
      insert = block + '\n';
      cursor = from + insert.length;
    } else {
      from = line.to;
      insert = '\n' + block;
      const next = line.number < doc.lines ? doc.line(line.number + 1) : null;
      if (!next || isImageLine(next)) {
        insert += '\n'; // a fresh line to type on
        cursor = from + insert.length;
      } else cursor = from + insert.length + 1; // start of the existing next line
    }
    view.dispatch({
      changes: { from, insert },
      selection: { anchor: cursor },
      scrollIntoView: true,
      userEvent: 'input.paste',
      annotations: imageOp.of(true),
    });
    view.focus();
  }

  /** Remove every marker line for ref (its upload was canceled). Not undoable: the upload is gone. */
  export function removeMarker(ref: string) {
    if (!view) return;
    retired.set(ref, null);
    const st = view.state;
    const doc = st.doc;
    const ranges: { from: number; to: number }[] = [];
    let first: Line | null = null;
    for (let i = 1; i <= doc.lines; i++) {
      const line = doc.line(i);
      if (MARKER_RE.exec(line.text)?.[1] !== ref) continue;
      first ??= line;
      const r = imageLineRange(doc, line);
      const prev = ranges[ranges.length - 1];
      if (prev && r.from <= prev.to) prev.to = Math.max(prev.to, r.to);
      else ranges.push(r);
    }
    if (!ranges.length) return;
    const changes = st.changes(ranges);
    // if the removed image was selected, land on text rather than on a neighbouring image
    const sel = selectedImage(st);
    const target = sel && first && sel.from === first.from ? textPos(doc, first, -1) : null;
    view.dispatch({
      changes,
      selection: target === null ? undefined : { anchor: changes.mapPos(target, -1) },
      annotations: [imageOp.of(true), Transaction.addToHistory.of(false)],
    });
  }

  /** Rewrite every marker for oldRef (the server rejected the ref as taken). */
  export function replaceRef(oldRef: string, newRef: string) {
    if (!view) return;
    retired.set(oldRef, newRef);
    const doc = view.state.doc;
    const changes = [];
    for (let i = 1; i <= doc.lines; i++) {
      const line = doc.line(i);
      if (MARKER_RE.exec(line.text)?.[1] === oldRef) changes.push({ from: line.from + 7, to: line.from + 7 + oldRef.length, insert: newRef });
    }
    if (changes.length) view.dispatch({ changes, annotations: [imageOp.of(true), Transaction.addToHistory.of(false)] });
  }
</script>

<div class="editor-host" class:raw bind:this={host}></div>
