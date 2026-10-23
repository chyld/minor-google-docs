// Browser checks for editing around inline images (SPEC.md §17).
// Runs against a live server with a throwaway user:
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/images.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

/** Where the selection is: {kind:'cursor', line, col} or {kind:'image', line}. */
const st = () =>
  page.evaluate(() => {
    const s = __v.state;
    const m = s.selection.main;
    const l = s.doc.lineAt(m.from);
    if (m.empty) return { kind: 'cursor', line: l.number, col: m.head - l.from };
    const isImg = m.from === l.from && m.to === l.to && l.text.startsWith('[image:');
    return isImg ? { kind: 'image', line: l.number } : { kind: 'range', from: m.from, to: m.to };
  });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const go = (line, col = 0) =>
  page.evaluate(
    ([line, col]) => {
      __v.focus();
      const l = __v.state.doc.line(line);
      __v.dispatch({ selection: { anchor: Math.min(l.from + col, l.to) }, scrollIntoView: true, userEvent: 'select' });
    },
    [line, col],
  );
const fmt = (s) => (s.kind === 'cursor' ? `L${s.line}` : s.kind === 'image' ? `I${s.line}` : 'range');
async function walk(key, n) {
  const out = [];
  for (let i = 0; i < n; i++) {
    await page.keyboard.press(key);
    out.push(fmt(await st()));
  }
  return out;
}

// ---- 1. vertical movement across consecutive images ----
{
  const id = await makeNote(['Consecutive', 'above', { img: 420 }, { img: 520 }, { img: 360 }, 'after one', 'after two']);
  await open(id);
  await go(7, 5);
  const up = await walk('ArrowUp', 6);
  check('ArrowUp through consecutive images visits every line and image', up.join(' ') === 'L6 I5 I4 I3 L2 L1', up);
  await go(1, 3);
  const down = await walk('ArrowDown', 6);
  check('ArrowDown through consecutive images visits every line and image', down.join(' ') === 'L2 I3 I4 I5 L6 L7', down);

  const drift = await page.evaluate(() => {
    const v = __v;
    let worst = 0;
    for (const el of v.contentDOM.children) {
      if (!el.matches('.cm-line, .cm-img')) continue;
      const b = v.lineBlockAt(v.posAtDOM(el));
      worst = Math.max(worst, Math.abs(el.getBoundingClientRect().top - v.documentTop - b.top));
    }
    return worst;
  });
  check('height map matches the page (≤ 2px) after images load', drift <= 2, drift);

  // Home/End and clicks on a text line between images stay on that line
  await go(6, 3);
  await page.keyboard.press('Home');
  const h = await st();
  await page.keyboard.press('End');
  const e = await st();
  check('Home/End stay on the text line below images', h.line === 6 && h.col === 0 && e.line === 6 && e.col === 9, { h, e });
  const box = await page.evaluate(() => {
    const l = __v.state.doc.line(6);
    const c = __v.coordsAtPos(l.from + 3);
    return { x: c.left + 2, y: (c.top + c.bottom) / 2 };
  });
  await page.mouse.click(box.x, box.y);
  check('clicking a text line between images puts the cursor there', (await st()).line === 6, await st());
}

// ---- 2. interleaved text and images ----
{
  const id = await makeNote(['Interleaved', 'intro', { img: 300 }, 'between 1', { img: 380 }, 'between 2', { img: 260 }, 'last']);
  await open(id);
  await go(8, 2);
  const up = await walk('ArrowUp', 7);
  check('ArrowUp through interleaved images', up.join(' ') === 'I7 L6 I5 L4 I3 L2 L1', up);
  await go(2, 0);
  const right = await walk('ArrowRight', 2);
  await go(2, 5);
  const r2 = await walk('ArrowRight', 2);
  check('ArrowRight at line end selects the next image, then steps to the text after it', r2.join(' ') === 'I3 L4', { right, r2 });
}

// ---- 3. Backspace / Delete next to images ----
{
  const body = ['Deleting', 'above', { img: 200 }, { img: 220 }, 'below text', ''];
  const id = await makeNote(body);
  await open(id);
  const before = await doc();
  await go(5, 0);
  await page.keyboard.press('Backspace');
  check('Backspace at the start of text after an image only selects it', fmt(await st()) === 'I4' && (await doc()) === before, await st());
  await page.keyboard.press('Backspace');
  const after1 = await doc();
  check('second Backspace deletes that image (no glue, no blank line)', after1.split('\n').length === before.split('\n').length - 1 && after1.includes('\nbelow text'), after1);
  check('after deleting, the cursor is on text (not on the other image)', (await st()).kind === 'cursor', await st());

  // held key: repeats never select or delete images
  await go(4, 0);
  const d0 = await doc();
  await page.evaluate(() => {
    for (let i = 0; i < 5; i++) __v.contentDOM.dispatchEvent(new KeyboardEvent('keydown', { key: 'Backspace', repeat: true, bubbles: true, cancelable: true }));
  });
  check('a held Backspace never deletes an image', (await doc()) === d0 && (await st()).kind === 'cursor', await st());

  // held Backspace on an empty line under an image stops there
  await page.evaluate(() => {
    const l = __v.state.doc.line(4); // 'below text', right under the image
    __v.dispatch({ changes: { from: l.from, insert: '\n' }, selection: { anchor: l.from } });
  });
  const dBlank = await doc();
  await page.evaluate(() => {
    for (let i = 0; i < 3; i++) __v.contentDOM.dispatchEvent(new KeyboardEvent('keydown', { key: 'Backspace', repeat: true, bubbles: true, cancelable: true }));
  });
  check('a held Backspace on an empty line under an image does not select it', (await doc()) === dBlank && (await st()).kind === 'cursor', await st());
  await page.evaluate(() => {
    const l = __v.state.doc.line(4);
    __v.dispatch({ changes: { from: l.from, to: l.from + 1 } });
  });

  // Delete at the end of the line above an image
  await go(2, 5);
  await page.keyboard.press('Delete');
  check('Delete at the end of text before an image only selects it', fmt(await st()) === 'I3' && (await doc()) === d0, await st());
  await page.keyboard.press('Escape');
  check('Escape leaves the image for the text below', (await st()).kind === 'cursor', await st());
}

// ---- 4. typing, Enter, Space, paste on a selected image ----
{
  const id = await makeNote(['Typing', 'above', { img: 240 }, 'below']);
  await open(id);
  await go(4, 0);
  await page.keyboard.press('ArrowUp');
  await page.keyboard.type('hi');
  let lines = (await doc()).split('\n');
  check('typing on a selected image starts a new line after it', lines[2].startsWith('[image:') && lines[3] === 'hi' && lines[4] === 'below', lines);

  await go(5, 0);
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('ArrowUp');
  check('reselected the image', fmt(await st()) === 'I3', await st());
  await page.keyboard.press('Shift+Enter');
  lines = (await doc()).split('\n');
  check('Shift+Enter on an image adds a line above it', lines[2] === '' && lines[3].startsWith('[image:') && fmt(await st()) === 'L3', lines);

  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  lines = (await doc()).split('\n');
  check('Enter on an image adds a line below it', lines[3].startsWith('[image:') && lines[4] === '' && fmt(await st()) === 'L5', lines);

  // composition (IME) and beforeinput (mobile keyboards) on a selected image
  await page.keyboard.press('ArrowUp');
  const n0 = (await doc()).split('\n').length;
  await page.evaluate(() => __v.contentDOM.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true })));
  check('starting IME composition on an image moves to a new line first', (await doc()).split('\n').length === n0 + 1 && (await st()).kind === 'cursor', await st());

  await page.keyboard.press('ArrowUp');
  check('reselected again', fmt(await st()) === 'I4', await st());
  await page.evaluate(() => {
    const dt = new DataTransfer();
    dt.setData('text/plain', 'pasted words');
    __v.contentDOM.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  });
  lines = (await doc()).split('\n');
  check('pasting text on a selected image puts it on a new line, image kept', lines[3].startsWith('[image:') && lines[4] === 'pasted words', lines);

  // Space opens the lightbox; Escape closes it and the editor has focus again
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press(' ');
  const lb = await page.waitForSelector('.lightbox', { timeout: 3000 }).then(() => true, () => false);
  check('Space on a selected image opens it', lb);
  await page.keyboard.press('Escape');
  await page.waitForTimeout(200);
  const focused = await page.evaluate(() => __v.hasFocus);
  await page.keyboard.press('ArrowDown');
  check('after closing the image, the editor has focus and arrows work', focused && (await st()).kind === 'cursor', { focused, st: await st() });
}

// ---- 5. mouse: click selects, second click opens ----
{
  const id = await makeNote(['Mouse', 'text', { img: 260 }, 'end']);
  await open(id);
  await go(4, 0);
  const fig = page.locator('figure.cm-img .cm-img-frame').first();
  await fig.click();
  const s1 = await st();
  const lb1 = await page.locator('.lightbox').count();
  check('clicking an image selects it (does not open it)', fmt(s1) === 'I3' && lb1 === 0, { s1, lb1 });
  const outlined = await page.evaluate(() => document.querySelector('figure.cm-img').classList.contains('selected'));
  check('the selected image is outlined', outlined);
  await fig.click();
  const lb2 = await page.waitForSelector('.lightbox', { timeout: 3000 }).then(() => true, () => false);
  check('clicking the selected image opens it', lb2);
  await page.keyboard.press('Escape');
  await page.waitForTimeout(200);
  check('focus returns to the editor', await page.evaluate(() => __v.hasFocus));
}

// ---- 6. pasting screenshots mid-document, then typing ----
{
  const id = await makeNote(['Paste', 'hello world', 'existing text']);
  await open(id);
  await go(2, 5);
  await page.evaluate(async () => {
    const c = document.createElement('canvas');
    c.width = 600;
    c.height = 300;
    c.getContext('2d').fillRect(0, 0, 600, 300);
    const f = new File([await new Promise((r) => c.toBlob(r, 'image/png'))], 'Screenshot.png', { type: 'image/png' });
    const dt = new DataTransfer();
    dt.items.add(f);
    __v.contentDOM.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  });
  await page.waitForTimeout(500);
  await page.keyboard.type('X');
  const lines = (await doc()).split('\n');
  check('after pasting a screenshot mid-note, typing goes to the next text line, not the caption', lines[2].startsWith('[image:') && /Screenshot|screenshot/.test(lines[2]) && !lines[2].endsWith('X') && lines[3] === 'Xexisting text', lines);
}

// ---- 7. trash (read-only) ----
{
  const id = await makeNote(['Trash', 'text', { img: 200 }, 'end']);
  await page.evaluate(async (id) => {
    await fetch(`/api/notes/${id}`, { method: 'PATCH', headers: { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' }, body: JSON.stringify({ trashed: true }) });
  }, id);
  await open(id);
  const d0 = await doc();
  await page.locator('figure.cm-img .cm-img-frame').first().click();
  const lb = await page.waitForSelector('.lightbox', { timeout: 3000 }).then(() => true, () => false);
  check('in the trash, one click opens an image', lb);
  await page.keyboard.press('Escape');
  await page.evaluate(() => {
    const l = __v.state.doc.line(3);
    __v.dispatch({ selection: { anchor: l.from, head: l.to } });
    for (const key of ['Backspace', 'Delete', 'Enter', 'x'])
      __v.contentDOM.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
  });
  check('in the trash, keys never change the note', (await doc()) === d0);
}

// ---- 7b. search and selections next to images ----
{
  const id = await makeNote(['Search', 'replace me', { img: 200 }, 'after']);
  await open(id);
  await go(4, 0);
  await page.keyboard.press('Control+f');
  await page.keyboard.type('shot2');
  await page.keyboard.press('Enter');
  await page.keyboard.press('Escape');
  await page.waitForTimeout(100);
  const d0 = await doc();
  await page.keyboard.press('Backspace');
  check('after finding text in a caption, Backspace selects the image instead of deleting it', (await doc()) === d0 && fmt(await st()) === 'I3', await st());

  const l2 = await page.evaluate(() => {
    const l = __v.state.doc.line(2);
    const c = __v.coordsAtPos(l.from + 2);
    return { x: c.left, y: (c.top + c.bottom) / 2 };
  });
  await page.mouse.click(l2.x, l2.y, { clickCount: 3 });
  await page.keyboard.type('X');
  const lines = (await doc()).split('\n');
  check('typing over a triple-clicked line above an image replaces it and keeps the image', lines[1] === 'X' && lines[2].startsWith('[image:') && lines[3] === 'after', lines);
}

// ---- 7c. Shift+arrows and PageDown from a selected image ----
{
  const id = await makeNote(['Shift', 'above', { img: 200 }, 'below']);
  await open(id);
  await go(4, 0);
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('Shift+ArrowUp');
  const r = await st();
  check('Shift+ArrowUp from an image selects it together with the line above', r.kind === 'range', r);
  const d0 = await doc();
  await page.keyboard.type('y');
  let lines = (await doc()).split('\n');
  check('typing over that selection replaces exactly what is selected (line above + image)', lines.join('|') === 'Shift|y|below', lines);
  await page.keyboard.press('Control+z');
  await page.waitForTimeout(100);
  check('undo restores it', (await doc()) === d0);
  await go(4, 0);
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('Shift+ArrowRight');
  await page.keyboard.type('x');
  lines = (await doc()).split('\n');
  check('Shift+ArrowRight then typing replaces the line below and keeps the image', lines[2].startsWith('[image:') && lines[3] === 'x' && lines.length === 4, lines);
  await page.keyboard.press('Control+z');
  await page.waitForTimeout(100);
  await go(4, 0);
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('Alt+l');
  await page.keyboard.type('z');
  lines = (await doc()).split('\n');
  check('select-line on an image then typing keeps the image and the next line', lines[2].startsWith('[image:') && lines[3] === 'z' && lines[4] === 'below', lines);
  await page.keyboard.press('Control+z');
  await page.waitForTimeout(100);
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('PageDown');
  check('PageDown from a selected image moves on', (await st()).kind === 'cursor', await st());
}

// ---- 7d. a copied image pasted at the very top goes after the title ----
{
  const id = await makeNote(['Title here', 'body', { img: 200 }]);
  await open(id);
  const marker = (await doc()).split('\n')[2];
  await go(1, 0);
  await page.evaluate((m) => {
    const dt = new DataTransfer();
    dt.setData('text/plain', m);
    __v.contentDOM.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  }, marker);
  const lines = (await doc()).split('\n');
  check('pasting an image at the top puts it after the title', lines[0] === 'Title here' && lines[1] === marker, lines);
}

// ---- 8. the reported flow: new note, paste screenshots one by one, type after, press Up ----
{
  await page.goto(BASE + '/');
  await page.waitForSelector('button:has-text("New")');
  const before = page.url();
  await page.click('button:has-text("New")');
  await page.waitForFunction((u) => location.href !== u && document.activeElement?.classList.contains('cm-content'), before);
  await page.waitForTimeout(200);
  await page.evaluate(() => {
    let o = document.querySelector('.cm-content').cmTile;
    while (o && !o.view) o = o.parent;
    window.__v = o.view;
  });
  await page.keyboard.type('Reported flow');
  await page.keyboard.press('Enter');
  await page.keyboard.type('Some text before.');
  await page.keyboard.press('Enter');
  for (const h of [420, 520, 360]) {
    await page.evaluate(async (h) => {
      const c = document.createElement('canvas');
      c.width = 900;
      c.height = h;
      c.getContext('2d').fillRect(0, 0, 900, h);
      const f = new File([await new Promise((r) => c.toBlob(r, 'image/png'))], 'Screenshot.png', { type: 'image/png' });
      const dt = new DataTransfer();
      dt.items.add(f);
      __v.contentDOM.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    }, h);
    await page.waitForTimeout(700);
  }
  await page.keyboard.type('First line after the screenshots.');
  await page.keyboard.press('Enter');
  await page.keyboard.type('Second line after.');
  await page.waitForTimeout(500);
  const lines = (await doc()).split('\n');
  check('pasted screenshots each sit on their own line, text after them intact',
    lines.length === 7 && lines.slice(2, 5).every((l) => l.startsWith('[image:')) && lines[5] === 'First line after the screenshots.' && lines[6] === 'Second line after.', lines);
  const up = await walk('ArrowUp', 6);
  check('ArrowUp from the last line walks back through text and every screenshot', up.join(' ') === 'L6 I5 I4 I3 L2 L1', up);

  // delete an image, then undo brings it back
  await go(6, 0);
  await page.keyboard.press('Backspace');
  await page.keyboard.press('Backspace');
  const afterDel = (await doc()).split('\n').length;
  await page.keyboard.press('Control+z');
  const afterUndo = (await doc()).split('\n');
  check('undo after deleting an image restores it', afterDel === 6 && afterUndo.length === 7 && afterUndo[4].startsWith('[image:'), { afterDel, afterUndo });
}

await finish();
