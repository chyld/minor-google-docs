// Browser checks for collapsible panels and page width (SPEC.md §18).
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/panels.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

const width = (sel) => page.evaluate((sel) => Math.round(document.querySelector(sel)?.getBoundingClientRect().width ?? -1), sel);
const pageW = () => width('.page');
const visible = (sel) => page.evaluate((sel) => {
  const el = document.querySelector(sel);
  return !!el && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden';
}, sel);
const maxW = () => page.evaluate(() => getComputedStyle(document.querySelector('.page')).maxWidth);
const hidden = async (sel) => !(await visible(sel));
const settle = () => page.waitForTimeout(150);

const id = await makeNote(['Panels test', 'Some text above.', { img: 300 }, 'Text right after the screenshot.', 'Last line.']);
await open(id);

// ---- defaults ----
check('by default the sidebar and the notes list are open', (await width('.sidebar')) === 248 && (await width('.list')) > 250);
check('the writing column starts at the normal width', (await maxW()) === '760px', await maxW());

// ---- sidebar → icon rail ----
await page.click('.sidebar .rail-toggle');
await settle();
check('collapsing the sidebar leaves a slim icon rail', (await width('.sidebar')) === 60, await width('.sidebar'));
check('the rail keeps the views, with tooltips', await page.evaluate(() => {
  const items = [...document.querySelectorAll('.sidebar .nav-item')].filter((b) => b.getClientRects().length);
  return items.length >= 6 && items.some((b) => b.title === 'Trash');
}));
check('tags are tucked away in the rail', !(await visible('.sidebar .tags')));
await page.reload();
await open(id);
check('the rail is remembered after a reload', (await width('.sidebar')) === 60);

// ---- notes list ----
await page.click('.list-head button[aria-label="Hide notes list"]');
await settle();
check('hiding the notes list gives its space to the note', (await hidden('.list')) && (await width('.main')) === 1220, await width('.main'));
check('a hidden list cannot be tabbed into', await page.evaluate(() => document.querySelector('.list').inert === true));
check('Auto page width widens while the list is hidden', (await pageW()) === 1100, await pageW());
check('the note bar offers to show the list again', await visible('.note-bar button[aria-label="Show notes list"]'));
await page.reload();
await open(id);
check('the hidden list is remembered after a reload', await hidden('.list'));

// cursor and clicks still line up after the column changed width
const aligned = await page.evaluate(() => {
  const l = __v.state.doc.line(4);
  const c = __v.coordsAtPos(l.from + 5);
  const r = document.createRange();
  const line = [...document.querySelectorAll('.cm-line')].find((el) => el.textContent.startsWith('Text right after'));
  r.setStart(line.firstChild, 5);
  r.setEnd(line.firstChild, 6);
  const b = r.getBoundingClientRect();
  return Math.abs(c.top - b.top) < 2 && Math.abs(c.left - b.left) < 2;
});
check('after widening, editor positions still match the text on screen', aligned);

await page.click('.note-bar button[aria-label="Show notes list"]');
await settle();
check('the note-bar button brings the list back', (await width('.list')) > 250 && (await maxW()) === '760px');

// ---- shortcuts ----
await page.click('.list .cards');
await page.keyboard.press('Control+Shift+Backslash');
await settle();
check('Ctrl+Shift+\\ hides the list', await hidden('.list'));
await page.keyboard.press('Control+Backslash');
await settle();
check('Ctrl+\\ expands the rail back to the full sidebar', (await width('.sidebar')) === 248);
await page.keyboard.press('Control+Backslash');
await settle();
check('…and collapses it again', (await width('.sidebar')) === 60);

// picking a view or searching brings the list back
await page.click('.sidebar .nav-item[aria-label="Pinned"]');
await settle();
check('picking a view in the rail shows the list again', (await width('.list')) > 250);
await page.click('.sidebar .nav-item[aria-label="All notes"]');
await page.keyboard.press('Control+Shift+Backslash');
await settle();
await page.click('.note-bar .status');
await page.keyboard.press('/');
await settle();
check('/ shows the list and focuses search', (await width('.list')) > 250 && (await page.evaluate(() => document.activeElement?.getAttribute('aria-label'))) === 'Search notes');

// ---- page width setting ----
async function setWidth(label) {
  await page.click('.note-bar button[aria-label="More actions"]');
  await page.click(`.menu [role="menuitemradio"]:text-is("${label}")`);
  await settle();
}
await open(id);
await setWidth('Wide');
check('Wide applies even with the list showing', (await maxW()) === '1100px', await maxW());
await setWidth('Normal');
await page.keyboard.press('Control+Shift+Backslash');
await settle();
check('Normal stays narrow even with the list hidden', (await pageW()) === 760);
await setWidth('Full');
check('Full uses the whole window', (await pageW()) >= 1150, await pageW());
await setWidth('Auto');

// ---- focus mode ----
await page.keyboard.press('Control+Shift+Backslash'); // list back
await settle();
await page.click('.note-bar .status');
await page.keyboard.press('Control+Shift+f');
await settle();
check('focus mode hides both panels', (await hidden('.sidebar')) && (await hidden('.list')) && (await width('.main')) === 1280);
await page.keyboard.press('Escape');
await settle();
check('Esc restores exactly the panels you had', (await width('.sidebar')) === 60 && (await width('.list')) > 250);

// ---- phones: unchanged ----
await page.setViewportSize({ width: 400, height: 800 });
await page.goto(BASE + '/');
await page.waitForSelector('.list .card');
check('on a phone the list fills the screen even if it was hidden on desktop', await page.evaluate(async () => {
  localStorage.setItem('notes.listCollapsed', '1');
  return true;
}));
await page.reload();
await page.waitForSelector('.list .card');
check('…the collapse settings are ignored there', (await width('.list')) === 400 && !(await visible('.list-head button[aria-label="Hide notes list"]')));
await page.click('.list-head button[aria-label="Open sidebar"]');
await settle();
check('…and the drawer still opens at full size', (await width('.sidebar')) > 250 && !(await page.evaluate(() => document.querySelector('.sidebar').classList.contains('rail'))));

await finish();
