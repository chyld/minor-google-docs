// Browser checks for bold and underline (SPEC §19: a highlight's style set).
//   PW=... BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/format.mjs
import fs from 'node:fs';
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const select = (text) =>
  page.evaluate((text) => {
    __v.focus();
    const i = __v.state.doc.toString().indexOf(text);
    __v.dispatch({ selection: { anchor: i, head: i + text.length }, userEvent: 'select' });
  }, text);
const settle = () => page.waitForTimeout(150);
const shown = (sel) => page.evaluate((sel) => [...document.querySelectorAll(sel)].some((e) => e.getClientRects().length), sel);
const style = (text) =>
  page.evaluate((text) => {
    const el = [...document.querySelectorAll('.cm-content span')].find((s) => s.textContent === text);
    if (!el) return null;
    const cs = getComputedStyle(el);
    return { weight: cs.fontWeight, deco: cs.textDecorationLine, bg: cs.backgroundColor, cls: el.className };
  }, text);

const id = await makeNote(['Format title words', 'alpha beta gamma', 'second line here', '## {teal} Section', 'inside text']);
await open(id);

// ---- the hover popup ----
await select('beta');
await settle();
check('the hover popup has Bold and Underline', (await shown('.hl-float button[aria-label="Bold"]')) && (await shown('.hl-float button[aria-label="Underline"]')));
await page.click('.hl-float button[aria-label="Bold"]');
await settle();
check('Bold wraps the selection as =={b}…==', (await doc()).includes('alpha =={b}beta== gamma'), await doc());
await page.evaluate(() => __v.dispatch({ selection: { anchor: 2 } })); // away from it: delimiters hide
await settle();
const b = await style('beta');
check('…drawn bold, no highlight tint, delimiters hidden', b?.weight === '700' && b.bg === 'rgba(0, 0, 0, 0)' && !(await page.evaluate(() => document.querySelector('.cm-content').innerText.includes('=={b}'))), b);
await select('beta');
await settle();
check('…and the button shows it is on', (await page.getAttribute('.hl-float button[aria-label="Bold"]', 'aria-pressed')) === 'true');
await page.click('.hl-float button[aria-label="Underline"]');
await settle();
check('Underline adds to the same span (=={b+u}…==)', (await doc()).includes('=={b+u}beta=='), await doc());
check('…drawn underlined and bold', (await style('beta'))?.deco === 'underline');
await page.click('.hl-float button[aria-label="Bold"]');
await settle();
check('Bold again takes bold off and keeps underline', (await doc()).includes('=={u}beta=='), await doc());

// colour keeps the formatting; "Remove colour" keeps it; "Clear formatting" removes everything
await page.click('.hl-float button[aria-label="Highlight colours"]');
await settle();
await page.click('.hl-pop-float .hl-swatch[aria-label="Coral"]');
await settle();
check('picking a colour keeps underline (=={coral+u}…==)', (await doc()).includes('=={coral+u}beta=='), await doc());
await select('beta');
await settle();
await page.click('.hl-float button[aria-label="Remove colour"]');
await settle();
check('"Remove colour" keeps underline', (await doc()).includes('=={u}beta=='), await doc());
await select('beta');
await settle();
await page.click('.hl-float button[aria-label="Clear formatting"]');
await settle();
check('"Clear formatting" removes everything', (await doc()).includes('alpha beta gamma'), await doc());

// a mixed selection: bold everywhere, then a second press removes it everywhere
await select('gamma');
await page.keyboard.press('Control+b');
await settle();
await page.evaluate(() => {
  const d = __v.state.doc.toString();
  __v.dispatch({ selection: { anchor: d.indexOf('alpha'), head: d.indexOf('here') + 4 }, userEvent: 'select' });
});
await settle();
check('a partly bold selection shows the button half-on', (await page.getAttribute('.hl-float button[aria-label="Bold"]', 'aria-pressed')) === 'mixed');
await page.keyboard.press('Control+b');
await settle();
check('Ctrl+B on a mixed selection makes all of it bold, line by line', (await doc()).includes('=={b}alpha beta gamma==\n=={b}second line here=='), await doc());
await page.keyboard.press('Control+b');
await settle();
check('…and again removes it from all', (await doc()).includes('alpha beta gamma\nsecond line here'), await doc());

// Ctrl+U, and the browser's own shortcuts never fire
const pagesBefore = page.context().pages().length;
await select('second');
await page.keyboard.press('Control+u');
await settle();
check('Ctrl+U underlines', (await doc()).includes('=={u}second=='), await doc());
await page.waitForTimeout(500);
check("…and the browser's Ctrl+U (view source) doesn't open", page.context().pages().length === pagesBefore);
await page.keyboard.press('Control+z');
await settle();
check('one undo step takes it back', (await doc()).includes('\nsecond line here'), await doc());

// a bare cursor stays in the text: Ctrl+B, then typing lands inside the (now bold) word
{
  const d = await doc();
  const at = d.indexOf('second') + 3;
  await page.evaluate((at) => (__v.focus(), __v.dispatch({ selection: { anchor: at }, userEvent: 'select' })), at);
  await page.keyboard.type('~'); // "sec~ond" is plain text: no span yet
  await page.keyboard.press('Control+z');
  await select('second');
  await page.keyboard.press('Control+b');
  const d2 = await doc();
  const mid = d2.indexOf('second') + 3;
  await page.evaluate((mid) => __v.dispatch({ selection: { anchor: mid }, userEvent: 'select' }), mid);
  await page.keyboard.press('Control+u');
  await page.keyboard.type('X');
  await settle();
  check('with just the cursor, Ctrl+U adds to the span and typing stays inside it', (await doc()).includes('=={b+u}secXond=='), await doc());
  await page.keyboard.press('Control+z');
  await page.keyboard.press('Control+z');
  await page.keyboard.press('Control+z');
  await settle();
}

// keyboard in the highlighter grid: Bold, then Underline, without leaving the menu
await select('second');
await page.click('.note-bar button[aria-label="Highlight"]');
await settle();
await page.focus('.hl-menu button[aria-label="Bold"]');
await page.keyboard.press('Enter');
await settle();
const still = await page.evaluate(() => !!document.activeElement?.closest('.hl-menu'));
await page.focus('.hl-menu button[aria-label="Underline"]');
await page.keyboard.press(' ');
await settle();
check('in the grid, Bold then Underline by keyboard, focus staying in the menu', still && (await doc()).includes('=={b+u}second=='), await doc());
await page.keyboard.press('Escape');
await page.keyboard.press('Control+z');
await page.keyboard.press('Control+z');
await settle();

// the title line stays the title's weight when bold
await select('Format');
await page.keyboard.press('Control+b');
await settle();
const title = await page.evaluate(() => {
  const s = [...document.querySelectorAll('.cm-title-line span')].find((x) => x.textContent === 'Format');
  return s && getComputedStyle(s).fontWeight;
});
check('bold in the title keeps the title weight (760)', title === '760', title);
await page.keyboard.press('Control+z');

// headings take no formatting; raw view does nothing
await select('Section');
await page.keyboard.press('Control+b');
await settle();
check('Ctrl+B on a section heading changes nothing', (await doc()).includes('## {teal} Section'), await doc());
await page.keyboard.press('Control+/');
await settle();
await select('inside');
await page.keyboard.press('Control+b');
await settle();
check('Ctrl+B in raw text changes nothing', (await doc()).includes('\ninside text'), await doc());
await page.keyboard.press('Control+/');
await settle();

// copy: plain text drops the delimiters
await select('inside');
await page.keyboard.press('Control+b');
await page.evaluate(() => {
  const d = __v.state.doc.toString();
  __v.dispatch({ selection: { anchor: d.lastIndexOf('\n') + 1, head: d.length }, userEvent: 'select' });
});
await page.keyboard.press('Control+c');
check('copy gives other apps the plain text', (await page.evaluate(() => navigator.clipboard.readText())) === 'inside text');

// palette
await select('here');
await page.keyboard.press('Control+k');
await page.keyboard.type('underline');
await page.waitForTimeout(200);
check('the palette offers Underline', await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].some((o) => /^underline/i.test(o.textContent.trim()))));
await page.keyboard.press('Enter');
await settle();
check('…and it underlines the selection', (await doc()).includes('=={u}here=='), await doc());

// the saved web page shows bold and underline
await page.evaluate(() => window.dispatchEvent(new CustomEvent('notes:save')));
await page.waitForTimeout(1200);
const page2 = await page.evaluate(async (id) => (await fetch(`/api/notes/${id}/html`, { headers: { 'X-Requested-With': 'notes' } })).text(), id);
check('the saved web page renders them', page2.includes('<span class="fmt-b">inside</span>') && page2.includes('<span class="fmt-u">here</span>'), page2.slice(page2.indexOf('<article'), page2.indexOf('</article>')));

// ---- phones: the highlighter grid ----
await page.setViewportSize({ width: 390, height: 800 });
await open(id);
await select('alpha');
await page.click('.note-bar button[aria-label="Highlight"]').catch(async () => page.click('button[title^="Highlight"]'));
await settle();
check('on a phone the highlighter menu has Bold and Underline', (await shown('.hl-pop-bar button[aria-label="Bold"]')) && (await shown('.hl-pop-bar button[aria-label="Underline"]')));
await page.click('.hl-pop-bar button[aria-label="Bold"]');
await settle();
check('…tapping Bold bolds and keeps the menu open (for Underline next)', (await doc()).includes('=={b}alpha==') && (await shown('.hl-pop-bar')), await doc());
await page.keyboard.press('Escape');
await page.setViewportSize({ width: 1280, height: 900 });

// the section colour picker stays colours only
await open(id);
await page.evaluate(() => window.dispatchEvent(new CustomEvent('notes:add-section')));
await settle();
check('the section colour picker has no Bold/Underline', (await shown('.sec-pop .hl-swatch')) && !(await shown('.sec-pop button[aria-label="Bold"]')));

// Firefox: Ctrl+B / Ctrl+U are browser shortcuts there (bookmarks sidebar, view source)
{
  const { createRequire } = await import('node:module');
  const { firefox } = createRequire(import.meta.url)(process.env.PW || 'playwright');
  let ff;
  try {
    ff = await firefox.launch();
  } catch {
    /* not installed */
  }
  if (!ff) check('firefox available for the shortcut check', false);
  else {
    const ctx = await ff.newContext();
    await ctx.addCookies(await page.context().cookies());
    const p = await ctx.newPage();
    await p.goto(`${BASE}/n/${id}`);
    await p.waitForSelector('.cm-content');
    await p.click('.cm-content');
    await p.evaluate(() => {
      let o = document.querySelector('.cm-content').cmTile;
      while (o && !o.view) o = o.parent;
      const v = o.view;
      window.__ff = v;
      const d = v.state.doc.toString();
      v.dispatch({ selection: { anchor: d.indexOf('gamma'), head: d.indexOf('gamma') + 5 }, userEvent: 'select' });
    });
    const before = ctx.pages().length;
    await p.keyboard.press('Control+b');
    await p.keyboard.press('Control+u');
    await p.waitForTimeout(700);
    const text = await p.evaluate(() => window.__ff.state.doc.toString());
    check('firefox: Ctrl+B then Ctrl+U format the text', text.includes('=={b+u}gamma=='), text);
    check('firefox: …and no view-source tab opens', ctx.pages().length === before);
    await ff.close();
  }
}

await finish();
