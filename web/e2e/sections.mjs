// Browser checks for sections (SPEC.md §22).
//   PW=/path/to/node_modules/playwright BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/sections.mjs
import { BASE, check, finish, makeNote, open, page } from './helpers.mjs';

await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
const doc = () => page.evaluate(() => __v.state.doc.toString());
const settle = () => page.waitForTimeout(150);
const shown = (sel) => page.evaluate((sel) => [...document.querySelectorAll(sel)].some((e) => e.getClientRects().length), sel);
const text = () => page.evaluate(() => document.querySelector('.cm-content').innerText);
const caret = (pos) => page.evaluate((pos) => (__v.focus(), __v.dispatch({ selection: { anchor: pos }, userEvent: 'select' })), pos);
const heads = () => page.evaluate(() => [...document.querySelectorAll('.cm-sec-head')].map((h) => h.dataset.sec));
const selected = () => page.evaluate(() => __v.state.sliceDoc(__v.state.selection.main.from, __v.state.selection.main.to));
const addBtn = '.note-bar button[aria-label="Add section"]';
const listBtn = '.note-bar button[aria-label="Sections"]';

// ---- adding from the note bar ----
const id = await makeNote(['Homelab router', 'Notes about the closet box.', 'The gateway is 10.0.0.1']);
await open(id);
check('no sections yet: no Sections button', !(await shown(listBtn)));
await caret((await doc()).indexOf('closet'));
await page.click(addBtn);
await settle();
check('Add section opens the colour grid', await page.evaluate(() => document.activeElement?.classList.contains('hl-swatch') && document.querySelector('.sec-pop [role=grid]')?.getAttribute('aria-label') === 'New section colour'));
await page.click('.sec-pop .hl-swatch[aria-label="Teal"]');
await settle();
check('picking a colour adds a heading after the cursor line', (await doc()) === 'Homelab router\nNotes about the closet box.\n## {teal} New section\nThe gateway is 10.0.0.1', await doc());
check('…with its name selected, ready to type', (await selected()) === 'New section');
await page.keyboard.type('Network');
check('typing names it', (await doc()).includes('\n## {teal} Network\n'));
check('it is drawn as a coloured heading', (await heads()).join() === 'teal');
await caret(0);
await settle();
check('away from it, "## {teal}" is hidden', !(await text()).includes('## {teal}') && (await text()).includes('Network'), await text());
check('the body has a bar in the section colour', await shown('.cm-sec-bar'));
await caret((await doc()).indexOf('Network') + 2);
await settle();
check('with the cursor on it, the markup shows (dimmed)', (await shown('.sec-delim')) && (await text()).includes('## {teal}'));
check('the Sections button appears', await shown(listBtn));

// a second one via the ⋯ menu, a different colour
await caret((await doc()).length);
await page.click('.note-bar button[aria-label="More actions"]');
await page.click('[role=menuitem]:has-text("Add section")');
await settle();
await page.click('.sec-pop .hl-swatch[aria-label="Coral, strong"]');
await page.keyboard.type('Logs');
await page.keyboard.press('Enter');
await page.keyboard.type('rotated nightly');
check('the ⋯ menu adds one too; Enter at its end starts the body', (await doc()).endsWith('\n## {coral-deep} Logs\nrotated nightly'), await doc());
check('two sections, two colours', (await heads()).join() === 'teal,coral-deep');

// ---- the section list and palette ----
await caret(0);
await page.click(listBtn);
await settle();
const items = await page.evaluate(() => [...document.querySelectorAll('.sec-list [role=menuitem]')].map((b) => b.textContent.trim()));
check('the list names every section', items.join('|') === 'Network|Logs', items);
await page.click('.sec-list [role=menuitem]:has-text("Logs")');
await settle();
check('picking one puts the cursor on its heading', await page.evaluate(() => __v.state.doc.lineAt(__v.state.selection.main.head).text === '## {coral-deep} Logs' && __v.hasFocus));
await page.keyboard.press('Control+k');
await page.keyboard.type('go to section net');
await settle();
await page.keyboard.press('Enter');
await settle();
check('the palette goes to a section', await page.evaluate(() => __v.state.doc.lineAt(__v.state.selection.main.head).text === '## {teal} Network'));

// ---- the heading menu: colour, move, collapse, remove ----
const openMenu = async (name) => {
  await page.evaluate((name) => {
    const h = [...document.querySelectorAll('.cm-sec-head')].find((l) => l.textContent.includes(name));
    h.querySelector('.sec-menu-btn').click();
  }, name);
  await settle();
};
await openMenu('Logs');
check('the ⋯ on a heading opens its menu', await shown('.sec-pop [role=menu]'));
await page.click('.sec-pop [role=menuitem]:has-text("Colour")');
await settle();
await page.click('.sec-pop .hl-swatch[aria-label="Violet"]');
await settle();
check('Colour… recolours it', (await doc()).includes('## {violet} Logs'));

await openMenu('Logs');
await page.click('.sec-pop [role=menuitem]:has-text("Move up")');
await settle();
check('Move up swaps it with the one above (the intro stays first)', (await doc()) === 'Homelab router\nNotes about the closet box.\n## {violet} Logs\nrotated nightly\n## {teal} Network\nThe gateway is 10.0.0.1', await doc());
await page.keyboard.press('Control+z');
check('one undo puts it back', (await doc()).startsWith('Homelab router\nNotes about the closet box.\n## {teal} Network'));

await openMenu('Network');
await page.click('.sec-pop [role=menuitem]:has-text("Collapse")');
await settle();
check('Collapse hides the body behind "⋯ 1 line"', !(await text()).includes('10.0.0.1') && (await shown('.sec-folded')), await text());
check('…the arrow turns', await page.evaluate(() => !!document.querySelector('.sec-chevron.folded')));
await open(id);
check('collapsed sections stay collapsed after a reload', await shown('.sec-folded'));
await page.click('.sec-chevron.folded');
await settle();
check('the arrow opens it again', !(await shown('.sec-folded')) && (await text()).includes('10.0.0.1'));
await page.click('.sec-chevron');
await settle();
await caret(0);
await page.keyboard.press('Control+f');
await page.keyboard.type('10.0.0');
await page.keyboard.press('Enter');
await settle();
check('Find opens a collapsed section with a match', !(await shown('.sec-folded')));
await page.keyboard.press('Escape');

await openMenu('Logs');
await page.click('.sec-pop [role=menuitem]:has-text("Remove section")');
await settle();
check('Remove section keeps its name and text', (await doc()).endsWith('\nLogs\nrotated nightly') && (await heads()).join() === 'teal', await doc());
await page.keyboard.press('Control+z');

// ---- editing rules in the browser ----
const nameStart = async (name) => (await doc()).indexOf(`} ${name}`) + 2;
await caret(await nameStart('Network'));
await page.keyboard.press('ArrowLeft');
check('ArrowLeft from the name start goes to the line above', await page.evaluate(() => __v.state.doc.lineAt(__v.state.selection.main.head).text === 'Notes about the closet box.'));
await page.keyboard.press('ArrowRight');
check('ArrowRight comes back to the name start (never inside the markup)', (await page.evaluate(() => __v.state.selection.main.head)) === (await nameStart('Network')));
await caret((await doc()).indexOf('The gateway'));
await page.keyboard.press('Backspace');
check('Backspace at the body start does not merge it into the heading', (await doc()).includes('## {teal} Network\nThe gateway'));
await caret(await nameStart('Network'));
await page.keyboard.press('Backspace');
check('Backspace at the name start turns the heading into text', (await doc()).includes('\nNetwork\nThe gateway') && !(await heads()).includes('teal'), await doc());
await page.keyboard.press('Control+z');
check('…undo brings it back', (await heads()).includes('teal'));

// highlights can't go into a heading
await page.evaluate(() => {
  const t = __v.state.doc.toString();
  __v.focus();
  __v.dispatch({ selection: { anchor: t.indexOf('Network'), head: t.indexOf('Network') + 7 }, userEvent: 'select' });
});
await settle();
check('the highlight button is off on a heading', await page.evaluate(() => document.querySelector('.note-bar button[aria-label="Highlight"]').disabled));

// copy: other apps get just the name
await page.evaluate(() => (__v.focus(), __v.dispatch({ selection: { anchor: 0, head: __v.state.doc.length } })));
await page.keyboard.press('Control+c');
const clip = await page.evaluate(() => navigator.clipboard.readText());
check('copied text has names, not markup', clip.includes('\nNetwork\n') && !clip.includes('## {'), clip);

// ---- title, preview, search ----
await page.keyboard.press('Control+s');
await page.waitForTimeout(900);
const card = await page.evaluate(() => document.querySelector('.card.selected .card-preview')?.textContent);
check('the list preview shows names without markup', !!card && card.includes('Network') && !card.includes('## {'), card);
const found = await page.evaluate(async () => (await (await fetch('/api/notes?q=network', { headers: { 'X-Requested-With': 'notes' } })).json()).items);
check('search finds a section name', found.some((n) => n.title === 'Homelab router'), found.map((n) => n.title));

// ---- raw text: markup is plain text, section tools are off ----
await page.keyboard.press('Control+/');
await settle();
check('raw text shows "## {teal} Network" as text', (await text()).includes('## {teal} Network') && !(await shown('.cm-sec-head')));
check('…and no section buttons', !(await shown(addBtn)) && !(await shown(listBtn)));
await page.keyboard.press('Control+/');
await settle();
check('back in the rendered view, sections are drawn again', (await heads()).length === 2);

// ---- an image in a section: one unbroken bar ----
const imgNote = await makeNote(['Pictures', '## {sky} Rack', 'before', { img: 160 }, 'after']);
await open(imgNote);
const bar = await page.evaluate(() => {
  const b = document.querySelector('.cm-sec-bar').getBoundingClientRect();
  const img = document.querySelector('.cm-img').getBoundingClientRect();
  return { top: b.top, bottom: b.bottom, imgTop: img.top, imgBottom: img.bottom };
});
check('the bar runs past an image in the section', bar.top < bar.imgTop && bar.bottom > bar.imgBottom, bar);

// ---- found in review ----
{
  const n = await makeNote(['Review', '## {teal} Alpha', 'unique-body-text', '## {sky} Beta', 'beta body']);
  await open(n);
  await openMenu('Alpha');
  await page.click('.sec-pop [role=menuitem]:has-text("Collapse")');
  await settle();
  // raw text and back keeps it collapsed (and remembered)
  await page.keyboard.press('Control+/');
  await settle();
  await page.keyboard.press('Control+/');
  await settle();
  check('raw text and back keeps a collapsed section collapsed', await shown('.sec-folded'));
  await open(n);
  check('…and remembered after a reload', await shown('.sec-folded'));
  // Find for the whole last line of a collapsed body opens it
  await caret(0);
  await page.keyboard.press('Control+f');
  await page.keyboard.type('unique-body-text');
  await page.keyboard.press('Enter');
  await settle();
  check('Find matching a collapsed body\'s last line opens it', !(await shown('.sec-folded')));
  await page.keyboard.press('Escape');
  await open(n);
  check('…and that stays open after a reload', !(await shown('.sec-folded')));
  // keyboard: the palette opens the menu of the section the cursor is in
  await caret((await doc()).indexOf('beta body'));
  await page.keyboard.press('Control+k');
  await page.keyboard.type('section options');
  await settle();
  await page.keyboard.press('Enter');
  await settle();
  check('the palette opens the current section\'s menu, focused', await page.evaluate(() => document.activeElement?.closest('.sec-pop [role=menu]') !== null && document.activeElement?.textContent.includes('Colour')));
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await settle();
  check('…and it works from the keyboard (Move up)', (await doc()).startsWith('Review\n## {sky} Beta\nbeta body\n## {teal} Alpha'), await doc());
  // a menu left open doesn't follow you to another note
  await openMenu('Alpha');
  await page.evaluate((id) => window.history.pushState({}, '', `/n/${id}`), id);
  await page.evaluate(() => window.dispatchEvent(new PopStateEvent('popstate')));
  await settle();
  check('switching notes closes a section menu', !(await shown('.sec-pop')));
}

// ---- round 2 of review ----
{
  const n = await makeNote(['Keys', '## {coral} Plain', 'p body', '## {coral-deep} Strong', 's body']);
  await open(n);
  const colors = await page.evaluate(() => [...document.querySelectorAll('.cm-sec-head')].map((h) => getComputedStyle(h).color));
  check('a "strong" colour looks different from the plain one', colors.length === 2 && colors[0] !== colors[1], colors);
  const nameAt = (await doc()).indexOf('Plain');
  await caret(nameAt);
  await page.keyboard.press('Home');
  check('Home at the name start stays there', (await page.evaluate(() => __v.state.selection.main.head)) === nameAt);
  await caret(nameAt + 5);
  await page.keyboard.press('Shift+Home');
  await page.keyboard.type('Renamed');
  check('Shift+Home then typing renames, keeping the heading', (await doc()).includes('\n## {coral} Renamed\n'), await doc());
  // a mouse click onto a heading is announced
  await caret((await doc()).length);
  await page.evaluate(() => [...document.querySelectorAll('.cm-sec-head')][1].scrollIntoView());
  const r = await page.evaluate(() => [...document.querySelectorAll('.cm-sec-head')][1].getBoundingClientRect().toJSON());
  await page.mouse.click(r.right - 60, r.top + r.height / 2);
  await settle();
  const said = await page.evaluate(() => document.querySelector('.cm-announced')?.textContent ?? '');
  check('clicking onto a heading announces the section', /Section, coral strong, Strong/.test(said), said);
}

// ---- round 3 of review: Tab, copying a heading ----
{
  const n = await makeNote(['Tabs', '## {mint} Shelf', 'body text', 'end line']);
  await open(n);
  await caret((await doc()).indexOf('Shelf') + 2);
  await page.keyboard.press('Tab');
  check('Tab on a heading leaves it a section', (await heads()).join() === 'mint' && (await doc()).includes('\n## {mint} Shelf\n'), await doc());
  // copy the whole name, paste on its own line: a section again; mid-line: just the name
  await page.evaluate(() => {
    const t = __v.state.doc.toString();
    __v.focus();
    __v.dispatch({ selection: { anchor: t.indexOf('Shelf'), head: t.indexOf('Shelf') + 5 }, userEvent: 'select' });
  });
  await page.keyboard.press('Control+c');
  await caret((await doc()).length);
  await page.keyboard.press('Enter');
  await page.keyboard.press('Control+v');
  await settle();
  check('a copied heading pasted on its own line is a section again', (await heads()).join() === 'mint,mint', await doc());
  await caret((await doc()).indexOf('body') + 2);
  await page.keyboard.press('Control+v');
  await settle();
  check('…pasted mid-line, it is just the name', (await doc()).includes('boShelfdy text'), await doc());
}

// ---- phones ----
await page.setViewportSize({ width: 360, height: 760 });
await open(id);
check('phone: section buttons move into the ⋯ menu', !(await shown(addBtn)));
await page.click('.note-bar button[aria-label="More actions"]');
check('…which has Add section and Sections', (await page.locator('[role=menuitem]:has-text("Add section")').isVisible()) && (await page.locator('[role=menuitem]:has-text("Sections")').isVisible()));
await page.click('[role=menuitem]:has-text("Sections")');
await settle();
const pop = await page.evaluate(() => document.querySelector('.sec-pop').getBoundingClientRect().toJSON());
check('…the list sits inside the screen', pop.left >= 0 && pop.right <= 360 && pop.bottom <= 760, pop);
await page.keyboard.press('Escape');

await finish();
