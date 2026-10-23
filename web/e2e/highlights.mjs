// Browser checks for text highlights (SPEC.md §19).
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/highlights.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const head = () => page.evaluate(() => __v.state.selection.main.head);
/** Select the first occurrence of text (as a user would). */
const select = (text) =>
  page.evaluate((text) => {
    __v.focus();
    const i = __v.state.doc.toString().indexOf(text);
    __v.dispatch({ selection: { anchor: i, head: i + text.length }, userEvent: 'select' });
  }, text);
const caret = (pos) => page.evaluate((pos) => (__v.focus(), __v.dispatch({ selection: { anchor: pos }, userEvent: 'select' })), pos);
const settle = () => page.waitForTimeout(120);
const shown = (sel) => page.evaluate((sel) => [...document.querySelectorAll(sel)].some((e) => e.getClientRects().length), sel);
const visibleText = () => page.evaluate(() => document.querySelector('.cm-content').innerText);

// ---- applying from the floating bar ----
const id = await makeNote(['Server notes', 'The admin password is hunter2 for now.', { img: 200 }, 'After the image.']);
await open(id);
check('the highlight button is disabled with nothing selected', await page.evaluate(() => document.querySelector('.note-bar button[aria-label="Highlight"]').disabled));
await select('password');
await settle();
check('selecting text shows the floating highlight bar (desktop)', await shown('.hl-float'));
check('…and enables the note-bar button', await page.evaluate(() => !document.querySelector('.note-bar button[aria-label="Highlight"]').disabled));
await page.click('.hl-float button[aria-label="Highlight colours"]');
await settle();
check('the colour grid opens with focus on a swatch', await page.evaluate(() => document.activeElement?.classList.contains('hl-swatch')));
check('it has 24 named colours', await page.evaluate(() => [...document.querySelectorAll('.hl-swatch')].filter((b) => b.getAttribute('aria-label')).length === 24));
await page.click('.hl-swatch[aria-label="Amber"]');
await settle();
check('picking a colour wraps the text', (await doc()).includes('=={amber}password=='), await doc());
check('the text shows coloured', await page.evaluate(() => {
  const m = document.querySelector('.cm-content .hl');
  return m?.textContent === 'password' && getComputedStyle(m).backgroundColor !== 'rgba(0, 0, 0, 0)';
}));
check('focus is back in the editor', await page.evaluate(() => __v.hasFocus));

// ---- hidden vs revealed delimiters ----
await caret(0);
await settle();
check('away from the highlight, its delimiters are hidden', !(await visibleText()).includes('=={amber}'), await visibleText());
const inner = (await doc()).indexOf('password') + 3;
await caret(inner);
await settle();
check('with the cursor in it, the delimiters show (dimmed)', (await shown('.hl-delim')) && (await visibleText()).includes('=={amber}'));

// typing at its edges
await caret((await doc()).indexOf('password') + 'password'.length);
await page.keyboard.type('X');
check('typing at the end of the coloured text extends it', (await doc()).includes('=={amber}passwordX=='));
await page.keyboard.press('ArrowRight');
await page.keyboard.type('Y');
check('one step right leaves it: typing goes after', (await doc()).includes('=={amber}passwordX==Y'), await doc());
await page.keyboard.press('Backspace');
await page.keyboard.press('Backspace');
check('Backspace right after it removes the highlight, keeping the text', (await doc()).includes('admin passwordX is'), await doc());
await page.keyboard.press('Control+z');
await page.keyboard.press('Control+z');
check('undo brings it back', (await doc()).includes('=={amber}passwordX==Y') || (await doc()).includes('=={amber}passwordX=='), await doc());

// arrows walk across a hidden highlight without stopping inside the delimiters
await page.evaluate(() => {
  const t = __v.state.doc.toString();
  __v.dispatch({ changes: { from: 0, to: t.length, insert: 'Title\nab =={sky}cd== ef' }, userEvent: 'input' });
});
await caret('Title\nab '.length - 1);
const walk = [];
for (let i = 0; i < 8; i++) {
  await page.keyboard.press('ArrowRight');
  walk.push(await head());
}
const d2 = await doc();
const okWalk = walk.every((p) => {
  const inOpen = p > d2.indexOf('=={sky}') && p < d2.indexOf('cd');
  const inClose = p > d2.indexOf('cd') + 2 && p < d2.indexOf('cd') + 4;
  return !inOpen && !inClose;
});
check('ArrowRight never rests inside a delimiter', okWalk, walk);

// Enter inside splits on both lines
await caret(d2.indexOf('cd') + 1);
await page.keyboard.press('Enter');
check('Enter inside a highlight continues it on the new line', (await doc()) === 'Title\nab =={sky}c==\n=={sky}d== ef', await doc());
await page.keyboard.type('Z');
check('…and typing there is inside the highlight', (await doc()) === 'Title\nab =={sky}c==\n=={sky}Zd== ef', await doc());

// ---- keyboard: Ctrl+Shift+H ----
await select('ef');
await page.keyboard.press('Control+Shift+h');
await settle();
check('Ctrl+Shift+H opens the colours', await shown('.hl-menu'));
await page.keyboard.press('Enter');
await settle();
check('Enter applies the focused (last used) colour', (await doc()).endsWith('=={amber}ef=='), await doc());
await select('ab');
await page.keyboard.press('Control+Shift+h');
await page.keyboard.press('Escape');
await settle();
check('Esc closes the colours and returns to the editor', !(await shown('.hl-menu')) && (await page.evaluate(() => __v.hasFocus)));

// ---- copy out: plain for other apps, highlights kept inside the app ----
await select('ab =={sky}c==');
await page.keyboard.press('Control+c');
const clip = await page.evaluate(() => navigator.clipboard.readText());
check('copied text has no delimiters', clip === 'ab c', clip);
await caret((await doc()).length);
await page.keyboard.press('Enter');
await page.keyboard.press('Control+v');
await settle();
check('pasting it inside the app keeps the highlight', (await doc()).endsWith('\nab =={sky}c=='), await doc());

// ---- clicking beside a highlight doesn't shift text under the mouse ----
await caret(0);
await settle();
const box = await page.evaluate(() => {
  const r = document.querySelector('.cm-content .hl[data-hl="amber"]').getBoundingClientRect();
  return { x: r.left + 4, y: r.top + r.height / 2 };
});
await page.mouse.move(box.x, box.y);
await page.mouse.down();
await settle();
const during = await shown('.hl-delim');
await page.mouse.up();
await settle();
check('delimiters appear only once the click is done', !during && (await shown('.hl-delim')));

// ---- title, preview, search and counts ignore delimiters ----
await page.evaluate(() => {
  const t = __v.state.doc.toString();
  __v.dispatch({ changes: { from: 0, to: t.length, insert: '=={coral}Router== settings\nThe =={mint}gateway== is 10.0.0.1' }, userEvent: 'input' });
});
await page.keyboard.press('Control+s');
await page.waitForTimeout(600);
const card = await page.evaluate((id) => {
  const c = [...document.querySelectorAll('.card')].find((c) => c.classList.contains('selected'));
  return { title: c?.querySelector('.card-title')?.textContent, preview: c?.querySelector('.card-preview')?.textContent };
}, id);
check('the list shows a clean title and preview', card.title === 'Router settings' && card.preview === 'The gateway is 10.0.0.1', card);
const words = await page.evaluate(() => document.querySelector('.note-foot span').textContent);
check('the word count ignores delimiters', words === '6 words', words);
await page.waitForTimeout(800); // indexer
const found = await page.evaluate(async () => (await (await fetch('/api/notes?q=gateway%20is', { headers: { 'X-Requested-With': 'notes' } })).json()).items);
check('search finds highlighted words across the delimiters', found.some((n) => n.title === 'Router settings' && n.snippet?.map((s) => s.text).join('').includes('gateway is')), found.map((n) => n.title));

// ---- found in review ----
{
  const id2 = await makeNote(['Review checks', { img: 120 }, '=={coral}after image== text', 'x'.repeat(70) + ' near the right edge words']);
  await open(id2);
  // the floating bar and its colours stay inside the window
  await select('edge words');
  await settle();
  await page.click('.hl-float button[aria-label="Highlight colours"]');
  await settle();
  const fit = await page.evaluate(() => {
    const r = document.querySelector('.hl-menu').getBoundingClientRect();
    const scrim = document.querySelector('.menu-scrim').getBoundingClientRect();
    return { right: r.right, w: innerWidth, scrimW: scrim.width, scrimH: scrim.height };
  });
  check('the colour menu stays inside the window', fit.right <= fit.w, fit);
  check('its click-away layer covers the whole window', fit.scrimW === 1280 && fit.scrimH === 900, fit);
  await page.mouse.click(5, 450);
  await settle();
  check('clicking elsewhere closes it', !(await shown('.hl-menu')));

  // the floating bar's Remove works
  await caret(0);
  await select('after image');
  await settle();
  await page.click('.hl-float button[aria-label="Remove highlight"]');
  await settle();
  check('Remove on the floating bar removes the highlight', (await doc()).includes('\nafter image text'), await doc());
  await page.keyboard.press('Control+z');

  // Backspace at the start of a highlighted line below an image selects the image
  const lineStart = (await doc()).indexOf('after image');
  await caret(lineStart);
  await page.keyboard.press('Backspace');
  const sel = await page.evaluate(() => {
    const r = __v.state.selection.main;
    return __v.state.sliceDoc(r.from, r.to).startsWith('[image:');
  });
  check('Backspace at the start of a highlighted line under an image selects the image', sel && (await doc()).includes('=={coral}after image=='), await doc());

  // copying a highlight with an image keeps both when pasted mid-line
  await page.evaluate(() => {
    const d = __v.state.doc;
    __v.focus();
    __v.dispatch({ selection: { anchor: d.line(2).from, head: d.line(3).to - ' text'.length }, userEvent: 'select' });
  });
  await page.keyboard.press('Control+c');
  await caret((await doc()).indexOf('near the') + 4);
  await page.keyboard.press('Control+v');
  await settle();
  check('pasting an image + highlight mid-line keeps the highlight', ((await doc()).match(/=={coral}after image==/g) || []).length === 2, await doc());

  // Esc in the colour menu doesn't also leave focus mode
  await page.keyboard.press('Control+Shift+f');
  await select('Review');
  await page.keyboard.press('Control+Shift+h');
  await settle();
  await page.keyboard.press('Escape');
  await settle();
  check('Esc closes the colours but stays in focus mode', !(await shown('.hl-menu')) && (await page.evaluate(() => !!document.querySelector('.shell.focus'))));
  await page.keyboard.press('Escape');
}

// ---- phones: no floating bar, the note-bar button still works ----
await page.setViewportSize({ width: 400, height: 800 });
await open(id);
await select('settings');
await settle();
check('no floating bar on a phone', !(await shown('.hl-float')));
await page.click('.note-bar button[aria-label="Highlight"]');
await settle();
check('…the note-bar button opens the colours', await shown('.hl-menu'));
const below = await page.evaluate(() => {
  document.querySelector('.hl-swatch[aria-label="Coral"]').focus();
  return true;
});
await page.keyboard.press('ArrowDown');
check('…arrow keys follow the 6-column grid on a phone', below && (await page.evaluate(() => document.activeElement?.getAttribute('aria-label'))) === 'Teal');
await page.click('.hl-swatch[aria-label="Pink, strong"]');
check('…and applies', (await doc()).includes('=={pink-deep}settings=='), await doc());

await finish();
