// Browser checks for the raw text view (SPEC.md §20).
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/raw.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const settle = () => page.waitForTimeout(120);
const shown = (sel) => page.evaluate((sel) => [...document.querySelectorAll(sel)].some((e) => e.getClientRects().length), sel);
const head = () => page.evaluate(() => __v.state.selection.main.head);
const visibleText = () => page.evaluate(() => document.querySelector('.cm-content').innerText);
const button = '.note-bar button[aria-label="Raw text"]';
const pressed = () => page.evaluate((b) => document.querySelector(b).getAttribute('aria-pressed') === 'true', button);
const select = (text) =>
  page.evaluate((text) => {
    __v.focus();
    const i = __v.state.doc.toString().indexOf(text);
    __v.dispatch({ selection: { anchor: i, head: i + text.length }, userEvent: 'select' });
  }, text);

const id = await makeNote(['Raw check', 'The =={amber}gateway== is up.', { img: 120 }, 'Last line']);
await open(id);
check('starts rendered: the image and colours are drawn', (await shown('.cm-img')) && (await shown('.cm-content .hl')) && !(await pressed()));

// ---- switching to raw ----
await page.click(button);
await settle();
check('the button turns raw text on', await pressed());
check('raw text shows the image line and delimiters as text', !(await shown('.cm-img')) && !(await shown('.cm-content .hl')) && (await visibleText()).includes('=={amber}gateway==') && (await visibleText()).includes('[image:'), await visibleText());
check('the highlight button is gone', !(await shown('.note-bar button[aria-label="Highlight"]')));
check('raw text uses the monospace font', await page.evaluate(() => {
  const mono = getComputedStyle(document.documentElement).getPropertyValue('--mono').trim();
  const f = getComputedStyle(document.querySelector('.cm-editor')).fontFamily;
  const first = (v) => v.split(',')[0].replace(/["'\s]/g, '');
  return !!mono && first(f) === first(mono);
}));

// editing the raw text as-is
await page.evaluate(() => {
  __v.focus();
  __v.dispatch({ selection: { anchor: __v.state.doc.toString().indexOf('gateway==') + 'gateway'.length + 1 } });
});
await page.keyboard.press('Backspace');
check('Backspace deletes a single delimiter character', (await doc()).includes('=={amber}gateway= is up.'), await doc());
await page.keyboard.type('=');
await page.keyboard.press('End');
await page.keyboard.type(' =={sky}typed==');
check('typed delimiters stay exactly as typed', (await doc()).includes('is up. =={sky}typed=='), await doc());
await select('Raw');
await settle();
check('selecting text shows no floating highlight bar', !(await shown('.hl-float')));
await page.keyboard.press('Control+Shift+h');
await settle();
check('Ctrl+Shift+H opens no colours', !(await shown('.hl-menu')));
await select('=={sky}typed==');
await page.keyboard.press('Control+c');
check('copying in raw text keeps the delimiters', (await page.evaluate(() => navigator.clipboard.readText())) === '=={sky}typed==');

// cursor inside the image line's text, then back to rendered
await page.evaluate(() => {
  __v.focus();
  const i = __v.state.doc.toString().indexOf('[image:') + 3;
  __v.dispatch({ selection: { anchor: i }, userEvent: 'select' });
});
await page.keyboard.press('Control+/');
await settle();
check('Ctrl+/ switches back to the rendered note', !(await pressed()) && (await shown('.cm-img')));
check('…the text typed in raw is coloured', await page.evaluate(() => [...document.querySelectorAll('.cm-content .hl')].some((e) => e.textContent === 'typed')));
check('…and the cursor no longer sits inside the image text', await page.evaluate(() => {
  const r = __v.state.selection.main;
  const line = __v.state.doc.lineAt(r.head);
  return !line.text.startsWith('[image:') || (r.from === line.from && r.to === line.to);
}));
await page.keyboard.press('Control+z');
check('undo reaches edits made in raw text', !(await doc()).includes('=={sky}typed=='), await doc());
await page.keyboard.press('Control+Shift+Z');

// saving, and remembering the choice
await page.keyboard.press('Control+/');
await page.keyboard.press('Control+s');
await page.waitForTimeout(600);
const saved = await page.evaluate(async (id) => (await (await fetch(`/api/notes/${id}`, { headers: { 'X-Requested-With': 'notes' } })).json()).body, id);
check('raw edits are saved', saved.includes('is up. =={sky}typed=='), saved);
await open(id);
check('raw text stays on after a reload', (await pressed()) && !(await shown('.cm-img')));
await page.click(button);
await settle();
check('…and off again', !(await pressed()) && (await shown('.cm-img')));

// the palette lists it
await page.keyboard.press('Control+k');
await page.keyboard.type('raw text');
await settle();
check('the command palette offers it', await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].some((o) => /raw text/i.test(o.textContent))));
await page.keyboard.press('Escape');

// ---- found in review ----
const isRaw = () => page.evaluate(() => localStorage.getItem('notes.raw') === '1');
const key = (init) => page.evaluate((init) => window.dispatchEvent(new KeyboardEvent('keydown', { ctrlKey: true, bubbles: true, cancelable: true, ...init })), init);
{
  await open(id);
  // the shortcut goes by the character typed, and only outside other text boxes
  await key({ key: '-', code: 'Slash' }); // German layout: Ctrl+- (zoom out) on the US "/" key
  await key({ key: '?', code: 'Slash', shiftKey: true });
  await key({ key: '/', repeat: true });
  check('Ctrl+- , Ctrl+? and a held key repeat do not switch', !(await isRaw()));
  await key({ key: '/', code: 'Digit7', shiftKey: true }); // German Ctrl+Shift+7
  check('Ctrl with the "/" character switches (any layout)', await isRaw());
  await key({ key: '/', code: 'KeyQ', altKey: true }); // AltGr layouts report Ctrl+Alt
  check('…including where "/" needs AltGr', !(await isRaw()));
  await page.focus('.list-head input');
  await page.keyboard.press('Control+/');
  check('Ctrl+/ in the search box does not switch the note', !(await isRaw()));
  await page.evaluate(() => __v.focus());
  await page.keyboard.press('Control+f');
  await page.waitForSelector('.cm-search input');
  await page.keyboard.press('Control+/');
  check('…nor in the find field', !(await isRaw()));
  await page.keyboard.press('Escape');

  // switching back and forth does not count as placing the cursor: an attachment still goes at the end
  await open(id);
  await page.click(button);
  await page.click(button);
  await page.waitForTimeout(300); // the button returns focus to the note
  const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');
  await page.setInputFiles('.note-bar input[type=file]', { name: 'dot.png', mimeType: 'image/png', buffer: png });
  await page.waitForFunction(() => __v.state.doc.toString().includes('dot.png'));
  const lines = (await doc()).split('\n');
  check('an attachment after switching twice still goes at the end', /dot\.png$/.test(lines[lines.length - 1]) || /dot\.png$/.test(lines[lines.length - 2]), lines);
  await page.keyboard.press('Control+z');

  // counts and clipboard across modes
  await select('gateway');
  await page.keyboard.press('Control+c');
  await page.keyboard.press('Control+/');
  await settle();
  await page.evaluate(() => (__v.focus(), __v.dispatch({ selection: { anchor: __v.state.doc.length } })));
  await page.keyboard.press('Enter');
  await page.keyboard.press('Control+v');
  check('a highlight copied in the rendered note pastes with its colour in raw text', (await doc()).endsWith('\n=={amber}gateway=='), await doc());
  await page.waitForTimeout(400);
  const chars = await page.evaluate(() => document.querySelector('.note-foot span:nth-child(2)').textContent);
  check('raw text counts the characters on screen', chars === `${(await doc()).length} characters`, chars);
  await page.keyboard.press('Control+z');

  // undo after switching back never leaves the cursor in a hidden delimiter
  await page.evaluate(() => {
    __v.focus();
    __v.dispatch({ selection: { anchor: __v.state.doc.toString().indexOf('gateway==') + 'gateway='.length } });
  });
  await page.keyboard.press('Backspace');
  await page.keyboard.press('Control+/');
  await settle();
  await page.keyboard.press('Control+z');
  await settle();
  const inDelim = await page.evaluate(() => {
    const t = __v.state.doc.toString();
    const h = __v.state.selection.main.head;
    const close = t.indexOf('gateway==') + 'gateway'.length;
    const open = t.indexOf('=={amber}');
    return (h > close && h < close + 2) || (h > open && h < open + 9);
  });
  check('undo in the rendered note keeps the cursor out of hidden delimiters', (await doc()).includes('=={amber}gateway==') && !inDelim, await head());

  // the colour grid closing because of the switch hands focus back to the note
  await select('Raw');
  await page.keyboard.press('Control+Shift+h');
  await settle();
  await page.keyboard.press('Control+/');
  await settle();
  check('switching with the colours open returns focus to the note', !(await shown('.hl-menu')) && (await page.evaluate(() => __v.hasFocus)));
  await page.keyboard.press('Control+/');
  await select('Raw');
  await settle();
  await page.focus('.note-bar button[aria-label="Highlight"]');
  await page.keyboard.press('Control+/');
  await settle();
  check('…and so does switching with focus on the highlight button', await page.evaluate(() => __v.hasFocus));
  await page.keyboard.press('Control+k');
  await page.keyboard.type('raw');
  await settle();
  check('in raw text the palette still finds it by "raw"', await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].some((o) => /raw text \(on\)/i.test(o.textContent))));
  await page.keyboard.press('Escape');

  // a Find match stays exactly the match across a switch (Backspace then selects the image, not deletes it)
  if (await isRaw()) await page.click(button);
  await page.evaluate(() => __v.focus());
  await page.keyboard.press('Control+f');
  await page.waitForSelector('.cm-search input');
  await page.keyboard.press('Control+a');
  await page.keyboard.type('shot2');
  await page.keyboard.press('Enter');
  await settle();
  const match = await page.evaluate(() => [__v.state.selection.main.from, __v.state.selection.main.to]);
  await page.click(button);
  await settle();
  const wentRaw = await isRaw();
  await page.click(button);
  await settle();
  const after = await page.evaluate(() => [__v.state.selection.main.from, __v.state.selection.main.to]);
  check('a Find match keeps its range through raw and back', wentRaw && !(await isRaw()) && after[0] === match[0] && after[1] === match[1], { match, after });
  await page.evaluate(() => __v.focus());
  const kept = await doc();
  await page.keyboard.press('Backspace');
  check('…so Backspace selects the image instead of deleting it', (await doc()) === kept && kept.includes('shot2.png'), { kept, now: await doc(), sel: await page.evaluate(() => __v.state.selection.main) });
  await page.keyboard.press('Escape');

  // every note, including trashed ones (read-only)
  const other = await makeNote(['Second note', 'plain body']);
  await page.evaluate(async (id) => fetch(`/api/notes/${id}`, { method: 'PATCH', headers: { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' }, body: JSON.stringify({ trashed: true }) }), other);
  await page.evaluate(() => localStorage.setItem('notes.raw', '1'));
  await open(other);
  check('a trashed note opens in raw text too, read-only', (await pressed()) && (await page.evaluate(() => __v.state.readOnly)));

  // phones: the toolbar fits, raw text lives in the ⋯ menu
  await open(id);
  for (const w of [320, 360]) {
    await page.setViewportSize({ width: w, height: 760 });
    await settle();
    const fit = await page.evaluate(() => {
      const bar = document.querySelector('.note-bar');
      const els = [...bar.querySelectorAll(':scope > button, :scope > .menu-wrap > button.icon-btn, :scope > .status')].filter((e) => e.getClientRects().length);
      const rs = els.map((e) => e.getBoundingClientRect());
      const overlap = rs.some((a, i) => rs.some((b, j) => i < j && a.right > b.left + 1 && b.right > a.left + 1));
      return { inside: rs.every((r) => r.right <= innerWidth + 0.5), overlap };
    });
    check(`at ${w}px the note bar fits without overlaps`, fit.inside && !fit.overlap, fit);
    for (const raw of [false, true]) {
      await page.evaluate((raw) => (__v.dom.parentElement.classList.toggle('raw', raw), document.querySelector('.page').classList.toggle('mono', !raw)), raw);
      const size = await page.evaluate(() => parseFloat(getComputedStyle(document.querySelector('.cm-editor')).fontSize));
      check(`at ${w}px the ${raw ? 'raw' : 'monospace'} font is at least 16px (no iOS zoom)`, size >= 16, size);
    }
    await page.evaluate(() => (document.querySelector('.page').classList.remove('mono'), __v.dom.parentElement.classList.toggle('raw', localStorage.getItem('notes.raw') === '1')));
  }
  check('…the raw button moves into the ⋯ menu', !(await shown(button)));
  const before = await isRaw();
  await page.click('.note-bar button[aria-label="More actions"]');
  await page.click('[role="menuitemcheckbox"]:has-text("Raw text")');
  await settle();
  check('…which switches it', (await isRaw()) !== before && (await shown('.cm-img')) === before);
  await page.evaluate(() => localStorage.setItem('notes.raw', '0'));
}

await finish();
