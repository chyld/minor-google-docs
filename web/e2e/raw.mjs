// Browser checks for the raw text view (SPEC.md §20).
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/raw.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const settle = () => page.waitForTimeout(120);
const shown = (sel) => page.evaluate((sel) => [...document.querySelectorAll(sel)].some((e) => e.getClientRects().length), sel);
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
check('raw text uses the monospace font', await page.evaluate(() => /mono/i.test(getComputedStyle(document.querySelector('.cm-editor')).fontFamily) || getComputedStyle(document.querySelector('.cm-editor')).fontFamily !== getComputedStyle(document.body).fontFamily));

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

await finish();
