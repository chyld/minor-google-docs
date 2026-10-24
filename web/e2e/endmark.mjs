// Browser checks for the end-of-note mark (SPEC.md §21).
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
check('a note with text ends with the end mark', await shown());
check('it sits below the last line', await page.evaluate((m) => {
  const lines = document.querySelectorAll('.cm-line');
  return document.querySelector(m).getBoundingClientRect().top >= lines[lines.length - 1].getBoundingClientRect().bottom;
}, mark));
check('it is hidden from screen readers and outside the editor', await page.evaluate((m) => {
  const e = document.querySelector(m);
  return e.getAttribute('aria-hidden') === 'true' && !e.closest('.cm-editor');
}, mark));
check('it is three faded grey dots', await page.evaluate((m) => {
  const s = getComputedStyle(document.querySelector(`${m} svg`));
  const muted = getComputedStyle(document.documentElement).getPropertyValue('--muted').trim();
  return document.querySelectorAll(`${m} circle`).length === 3 && s.fill !== 'none' && Number(s.opacity) < 1 && !!muted;
}, mark));

check('a faded hairline either side, each ending in a small diamond', await page.evaluate((m) => {
  const rules = [...document.querySelectorAll(`${m} .end-rule`)];
  const dots = document.querySelector(`${m} svg`).getBoundingClientRect();
  const host = document.querySelector('.editor-host').getBoundingClientRect();
  const [a, b] = rules.map((r) => r.getBoundingClientRect());
  const cap = (r) => getComputedStyle(r, '::after');
  return rules.length === 2 && a.right <= dots.left && b.left >= dots.right && a.width > 100 && Math.abs(a.width - b.width) < 2 &&
    a.left >= host.left - 1 && b.right <= host.right + 1 && Number(getComputedStyle(rules[0], '::before').opacity) < 1 &&
    rules.every((r) => cap(r).width === '5px' && cap(r).transform !== 'none');
}, mark));

await page.evaluate(() => __v.focus());
await page.keyboard.press('Control+a');
await page.keyboard.press('Control+c');
check('select-all + copy gives only the text', (await page.evaluate(() => navigator.clipboard.readText())) === (await doc()));
check('…and the saved text has nothing extra', (await page.evaluate(async (id) => (await (await fetch(`/api/notes/${id}`, { headers: { 'X-Requested-With': 'notes' } })).json()).body, id)) === 'Wave check\nfirst line\nlast line');

// the colour follows the note's first tag
const tagged = await page.evaluate(async (id) => {
  const H = { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' };
  const t = await (await fetch('/api/tags', { method: 'POST', headers: H, body: JSON.stringify({ name: `endmark-${Date.now()}`, color: 'coral' }) })).json();
  await fetch(`/api/notes/${id}`, { method: 'PATCH', headers: H, body: JSON.stringify({ tag_ids: [t.id ?? t.tag?.id] }) });
  return true;
}, id);
await open(id);
const hues = await page.evaluate((m) => ({ h: getComputedStyle(document.querySelector('.note')).getPropertyValue('--accent-h').trim(), stroke: getComputedStyle(document.querySelector(`${m} svg`)).stroke }), mark);
const fillTagged = await page.evaluate((m) => getComputedStyle(document.querySelector(`${m} svg`)).fill, mark);
const fillPlain = await page.evaluate(() => {
  const e = document.createElement('div');
  e.style.color = 'var(--muted)';
  document.body.append(e);
  const c = getComputedStyle(e).color;
  e.remove();
  return c;
});
check('with a tag, it stays neutral grey (not the tag colour)', tagged && hues.h !== '295' && fillTagged === fillPlain, { hues, fillTagged, fillPlain });

// ---- clicking it, or the empty page below, goes to the end ----
await page.evaluate(() => __v.dispatch({ selection: { anchor: 0 } }));
const b = await box();
await page.mouse.click(b.x + b.width / 2, b.y + b.height / 2);
await settle();
check('clicking the end mark puts the cursor at the end', await page.evaluate(() => __v.hasFocus && __v.state.selection.main.head === __v.state.doc.length));
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

// ---- found in review: only a real click there jumps ----
const sel = () => page.evaluate(() => ({ from: __v.state.selection.main.from, to: __v.state.selection.main.to }));
const hostBox = () => page.evaluate(() => document.querySelector('.editor-host').getBoundingClientRect().toJSON());
{
  // a drag from the text that ends below the note keeps its selection
  const start = await page.evaluate(() => {
    const r = document.querySelectorAll('.cm-line')[1].getBoundingClientRect();
    return { x: r.left + 4, y: r.top + r.height / 2 };
  });
  const end = await box();
  await page.mouse.move(start.x, start.y);
  await page.mouse.down();
  await page.mouse.move(end.x + end.width / 2, end.y + end.height + 20, { steps: 8 });
  await page.mouse.up();
  await settle();
  const s1 = await sel();
  check('a drag from the text ending below the note keeps its selection', s1.to - s1.from > 5, s1);

  // Shift+click below extends the selection to the end
  await page.evaluate(() => (__v.focus(), __v.dispatch({ selection: { anchor: 3 } })));
  const wb = await box();
  await page.keyboard.down('Shift');
  await page.mouse.click(wb.x + wb.width / 2, wb.y + wb.height / 2);
  await page.keyboard.up('Shift');
  await settle();
  check('Shift+click on the end mark extends the selection to the end', await page.evaluate(() => {
    const r = __v.state.selection.main;
    return r.anchor === 3 && r.head === __v.state.doc.length;
  }), await sel());

  // the side margins under the note don't jump either
  await page.evaluate(() => (__v.focus(), __v.dispatch({ selection: { anchor: 3 } })));
  const hb = await hostBox();
  await page.mouse.click(hb.left - 20, hb.bottom + 30);
  await settle();
  check('a click in the side margin under the note does not jump', (await sel()).from === 3, await sel());

  // a short note in a tall window: the whole empty area below counts
  await page.setViewportSize({ width: 1280, height: 1600 });
  await settle();
  await page.evaluate(() => (__v.focus(), __v.dispatch({ selection: { anchor: 3 } })));
  const far = await page.evaluate(() => {
    const h = document.querySelector('.editor-host').getBoundingClientRect();
    const sc = document.querySelector('.note-scroll').getBoundingClientRect();
    const pg = document.querySelector('.page').getBoundingClientRect();
    return { x: h.left + 30, y: sc.bottom - 30, outsidePage: sc.bottom - 30 > pg.bottom };
  });
  await page.mouse.click(far.x, far.y);
  await settle();
  check('in a tall window, a click far below a short note goes to the end', far.outsidePage && (await page.evaluate(() => __v.state.selection.main.head === __v.state.doc.length)), far);
  await page.setViewportSize({ width: 1280, height: 900 });
  await settle();
}

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
check('after a final image, clicking the end mark selects the image', await page.evaluate(() => {
  const r = __v.state.selection.main;
  return __v.state.sliceDoc(r.from, r.to).startsWith('[image:') && r.to === __v.state.doc.length;
}));
await page.keyboard.type('caption below');
check('…and typing starts a line under it', (await doc()).endsWith('.png\ncaption below'), await doc());

// in raw text the cursor goes to the true end of the text (the image line is just text there)
await page.keyboard.press('Control+/');
await settle();
const rb = await box();
await page.mouse.click(rb.x + rb.width / 2, rb.y + rb.height / 2);
await settle();
check('raw text: clicking the end mark puts the cursor at the very end', await page.evaluate(() => __v.state.selection.main.empty && __v.state.selection.main.head === __v.state.doc.length));
await page.keyboard.press('Control+/');

// trashed notes show the end mark, but clicking it does nothing
const trashed = await makeNote(['Old note', 'body text']);
await page.evaluate(async (id) => fetch(`/api/notes/${id}`, { method: 'PATCH', headers: { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' }, body: JSON.stringify({ trashed: true }) }), trashed);
await open(trashed);
check('a trashed note shows the end mark', await shown());
await page.evaluate(() => __v.dispatch({ selection: { anchor: 2 } }));
const tb = await box();
await page.mouse.click(tb.x + tb.width / 2, tb.y + tb.height / 2);
await settle();
check('…clicking it there does nothing (read-only)', (await page.evaluate(() => __v.state.selection.main.head)) === 2 && (await page.evaluate((m) => getComputedStyle(document.querySelector(m)).cursor, mark)) === 'default');

// ---- not on an empty note; in raw text too ----
const empty = await makeNote(['']);
await open(empty);
check('an empty note shows no end mark', !(await shown()));
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
check('dark theme: still drawn', await page.evaluate((m) => getComputedStyle(document.querySelector(`${m} svg`)).fill !== 'none', mark));
await page.setViewportSize({ width: 390, height: 800 });
await open(id);
check('phones: shown, centred', await page.evaluate((m) => {
  const r = document.querySelector(`${m} svg`).getBoundingClientRect();
  return Math.abs(r.left + r.width / 2 - innerWidth / 2) < 30;
}, mark));

await finish();
