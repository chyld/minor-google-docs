<script lang="ts">
  import { onMount } from 'svelte';
  import { EditorState, Annotation } from '@codemirror/state';
  import { EditorView, keymap, placeholder } from '@codemirror/view';
  import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
  import { search, searchKeymap } from '@codemirror/search';
  import { markers } from '../lib/editor/markers';
  import { stamp } from '../lib/format';
  import { markerLine } from '../lib/ids';

  interface Props {
    readonly?: boolean;
    onchange: (text: string) => void;
    onfiles: (files: File[], at: 'cursor' | number) => void;
    onsave: () => void;
    onblur: () => void;
  }
  let { readonly = false, onchange, onfiles, onsave, onblur }: Props = $props();

  let host: HTMLDivElement;
  let view: EditorView | null = null;
  const external = Annotation.define<boolean>();

  function extensions(ro: boolean) {
    return [
      history(),
      search({ top: true }),
      markers(),
      EditorView.lineWrapping,
      placeholder('Start with a title…'),
      EditorState.readOnly.of(ro),
      EditorView.editable.of(!ro),
      keymap.of([
        { key: 'Mod-s', preventDefault: true, run: () => (onsave(), true) },
        {
          key: 'Alt-t',
          run: (v) => {
            v.dispatch(v.state.replaceSelection(stamp()));
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
      }),
    ];
  }

  onMount(() => {
    view = new EditorView({ parent: host, state: EditorState.create({ doc: '', extensions: extensions(readonly) }) });
    return () => view?.destroy();
  });

  /** Replace the whole document without triggering onchange (loading a note). */
  export function setDoc(text: string, ro = false) {
    if (!view) return;
    view.setState(EditorState.create({ doc: text, extensions: extensions(ro) }));
  }

  /** Replace content as an edit (undoable, triggers onchange). */
  export function replaceAll(text: string) {
    view?.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
  }

  export function getDoc(): string {
    return view?.state.doc.toString() ?? '';
  }

  export function focus(atEnd = false) {
    if (!view) return;
    view.focus();
    if (atEnd) view.dispatch({ selection: { anchor: view.state.doc.length } });
  }

  /** Insert marker lines for images on their own lines at pos (or the cursor). */
  export function insertMarkers(items: { ref: string; filename: string }[], at: 'cursor' | number) {
    if (!view || !items.length) return;
    const st = view.state;
    const pos = at === 'cursor' ? st.selection.main.head : at;
    const line = st.doc.lineAt(pos);
    const block = items.map((i) => markerLine(i.ref, i.filename)).join('\n');
    let from: number, to: number, insert: string;
    if (line.text.trim() === '') {
      from = line.from;
      to = line.to;
      insert = block + (line.to === st.doc.length ? '\n' : '');
    } else if (pos === line.from) {
      from = to = line.from;
      insert = block + '\n';
    } else {
      from = to = line.to;
      insert = '\n' + block + (line.to === st.doc.length ? '\n' : '');
    }
    view.dispatch({ changes: { from, to, insert }, selection: { anchor: from + insert.length }, scrollIntoView: true });
    view.focus();
  }

  /** Remove the marker line for ref (its upload was canceled). */
  export function removeMarker(ref: string) {
    if (!view) return;
    const doc = view.state.doc;
    for (let i = 1; i <= doc.lines; i++) {
      const line = doc.line(i);
      if (line.text.startsWith(`[image:${ref}]`)) {
        const to = line.to < doc.length ? line.to + 1 : line.to;
        const from = line.to < doc.length || line.from === 0 ? line.from : line.from - 1;
        view.dispatch({ changes: { from, to } });
        return;
      }
    }
  }

  /** Rewrite a marker ref (used when the server rejected a ref as taken). */
  export function replaceRef(oldRef: string, newRef: string) {
    if (!view) return;
    const doc = view.state.doc.toString();
    const needle = `[image:${oldRef}]`;
    const i = doc.indexOf(needle);
    if (i >= 0) view.dispatch({ changes: { from: i, to: i + needle.length, insert: `[image:${newRef}]` } });
  }
</script>

<div class="editor-host" bind:this={host}></div>
