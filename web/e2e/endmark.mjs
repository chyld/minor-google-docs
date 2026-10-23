// Browser checks for the end-of-note wave (SPEC.md §21).
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/endmark.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const settle = () => page.waitForTimeout(150);
const mark = '.note-end';
const shown = () => page.evaluate((m) => !!document.querySelector(m)?.getClientRects().length, mark);
const box = () => page.evaluate((m) => document.querySelector(m).getBoundingClientRect().toJSON(), mark);

// ---- shown under the text, not part of it ----
const id = await makeNote(['Wave check', 'first line', 'last line']);
await open(id);
check('a note with text ends with the wave', await shown());
check('it sits below the last line', await page.evaluate((m) => {
  const lines = document.querySelectorAll('.cm-line');
  return document.querySelector(m).getBoundingClientRect().top >= lines[lines.length - 1].getBoundingClientRect().bottom;
}, mark));
check('it is hidden from screen readers and outside the editor', await page.evaluate((m) => {
  const e = document.querySelector(m);
  return e.getAttribute('aria-hidden') === 'true' && !e.closest('.cm-editor');
}, mark));
check('it is drawn in the note colour, faded', await page.evaluate((m) => {
  const s = getComputedStyle(document.querySelector(`${m} svg`));
  return s.stroke !== 'none' && Number(s.opacity) < 1;
}, mark));

await page.evaluate(() => __v.focus());
await page.keyboard.press('Control+a');
await page.keyboard.press('Control+c');
check('select-all + copy gives only the text', (await page.evaluate(() => navigator.clipboard.readText())) === (await doc()));
check('…and the saved text has nothing extra', (await page.evaluate(async (id) => (await (await fetch(`/api/notes/${id}`, { headers: { 'X-Requested-With': 'notes' } })).json()).body, id)) === 'Wave check\nfirst line\nlast line');

// ---- clicking it, or the empty page below, goes to the end ----
await page.evaluate(() => __v.dispatch({ selection: { anchor: 0 } }));
const b = await box();
await page.mouse.click(b.x + b.width / 2, b.y + b.height / 2);
await settle();
check('clicking the wave puts the cursor at the end', await page.evaluate(() => __v.hasFocus && __v.state.selection.main.head === __v.state.doc.length));
await page.keyboard.type('!');
check('…so typing continues the last line', (await doc()).endsWith('last line!'), await doc());

await page.evaluate(() => __v.dispatch({ selection: { anchor: 0 } }));
const below = await page.evaluate((m) => {
  const r = document.querySelector(m).getBoundingClientRect();
  const page = document.querySelector('.page').getBoundingClientRect();
  return { x: page.left + 60, y: Math.min(r.bottom + 30, page.bottom - 4) };
}, mark);
await page.mouse.click(below.x, below.y);
await settle();
check('clicking the empty page under the note does too', await page.evaluate(() => __v.state.selection.main.head === __v.state.doc.length));

// a click beside the text (page margin) does not jump
await page.evaluate(() => __v.dispatch({ selection: { anchor: 3 } }));
const side = await page.evaluate(() => {
  const r = document.querySelector('.cm-line').getBoundingClientRect();
  const p = document.querySelector('.page').getBoundingClientRect();
  return { x: p.left + 8, y: r.top + r.height / 2 };
});
await page.mouse.click(side.x, side.y);
await settle();
check('a click in the side margin does not jump to the end', await page.evaluate(() => __v.state.selection.main.head !== __v.state.doc.length));

// ---- grows with the text, follows the editor ----
const y0 = (await box()).y;
await page.evaluate(() => (__v.focus(), __v.dispatch({ selection: { anchor: __v.state.doc.length } })));
await page.keyboard.press('Enter');
await page.keyboard.type('one more line');
await settle();
check('it moves down as lines are added', (await box()).y > y0);

// ---- a note ending with an image: the image gets selected, typing goes below it ----
const imgNote = await makeNote(['Picture note', 'text', { img: 120 }]);
await open(imgNote);
const ib = await box();
await page.mouse.click(ib.x + ib.width / 2, ib.y + ib.height / 2);
await settle();
check('after a final image, clicking the wave selects the image', await page.evaluate(() => {
  const r = __v.state.selection.main;
  return __v.state.sliceDoc(r.from, r.to).startsWith('[image:') && r.to === __v.state.doc.length;
}));
await page.keyboard.type('caption below');
check('…and typing starts a line under it', (await doc()).endsWith('.png\ncaption below'), await doc());

// ---- not on an empty note; in raw text too ----
const empty = await makeNote(['']);
await open(empty);
check('an empty note shows no wave', !(await shown()));
await page.evaluate(() => __v.focus());
await page.keyboard.type('Now a title');
await settle();
check('…until it has text', await shown());
await page.keyboard.press('Control+/');
await settle();
check('raw text has it too', await shown());
await page.keyboard.press('Control+/');

// ---- dark theme and phones ----
await page.emulateMedia({ colorScheme: 'dark' });
await page.evaluate(() => (document.documentElement.dataset.theme = 'dark'));
await settle();
check('dark theme: still drawn', await page.evaluate((m) => getComputedStyle(document.querySelector(`${m} svg`)).stroke !== 'none', mark));
await page.setViewportSize({ width: 390, height: 800 });
await open(id);
check('phones: shown, centred', await page.evaluate((m) => {
  const r = document.querySelector(`${m} svg`).getBoundingClientRect();
  return Math.abs(r.left + r.width / 2 - innerWidth / 2) < 30;
}, mark));

await finish();
