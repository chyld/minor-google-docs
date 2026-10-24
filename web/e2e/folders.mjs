// Browser checks for one folder per note (SPEC §3, §6.3, §9.2): pasted images get
// their own copy, "Check my notes", and the export button.
//   PW=... BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... DATA=/path/to/data node web/e2e/folders.mjs
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { check, finish, makeNote, open, page } from './helpers.mjs';

const DATA = process.env.DATA;
const settle = () => page.waitForTimeout(200);
const H = { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' };
const api = (method, url, body) =>
  page.evaluate(async ([method, url, body, H]) => {
    const r = await fetch(url, { method, headers: H, body: body ? JSON.stringify(body) : undefined });
    return r.status === 204 ? null : r.json();
  }, [method, url, body, H]);
const tag = Date.now().toString(36);

// ---- a pasted image gets its own copy, and survives the original's deletion ----
const src = await makeNote([`Source ${tag}`, 'has the picture', { img: 90 }]);
const ref = (await api('GET', `/api/notes/${src}`)).attachments[0].ref;
const dst = await makeNote([`Target ${tag}`, 'borrows it']);
await open(dst);
await page.evaluate((ref) => {
  const end = __v.state.doc.length;
  __v.dispatch({ changes: { from: end, insert: `\n[image:${ref}] pasted.png` } });
}, ref);
// autosave, then the server copies the image into this note's folder
await page.waitForFunction(() => document.querySelector('.count-badge')?.textContent === '1', null, { timeout: 8000 }).catch(() => {});
const copy = (await api('GET', `/api/notes/${dst}`)).attachments;
check('saving a pasted image gives the note its own copy (same ref)', copy.length === 1 && copy[0].ref === ref && copy[0].note_id === dst, copy);
check('…and the attachments badge updates without reloading', (await page.textContent('.count-badge').catch(() => '')) === '1');
check('the image shown now comes from this note\'s copy', await page.evaluate((id) => document.querySelector('.cm-img img')?.getAttribute('src')?.includes(id), copy[0].id));
if (DATA) {
  const hit = fs.globSync(path.join(DATA, 'users', '*', 'notes', dst, 'files', `${ref}.png`));
  check('on disk it is files/<ref>.png inside the note\'s folder', hit.length === 1, hit);
}

await api('PATCH', `/api/notes/${src}`, { trashed: true });
await api('DELETE', `/api/notes/${src}`);
// redraw the widgets (raw text on and off) so the image is loaded again
await page.keyboard.press('Control+/');
await settle();
await page.keyboard.press('Control+/');
await page.waitForFunction(() => { const i = document.querySelector('.cm-img img'); return i && i.complete; }, null, { timeout: 5000 }).catch(() => {});
check('after the original note is deleted forever, the image still shows', await page.evaluate(() => {
  const i = document.querySelector('.cm-img img');
  return !!i && i.naturalWidth > 0 && document.querySelector('.cm-img')?.dataset.state !== 'broken';
}));

// ---- Check my notes ----
await page.keyboard.press('Control+k');
await page.keyboard.type('check my notes');
await settle();
check('the palette offers "Check my notes"', await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].some((o) => /check my notes/i.test(o.textContent))));
await page.keyboard.press('Enter');
await page.waitForSelector('.check-result p', { timeout: 10000 }).catch(() => {});
check('running it from the palette opens Account & data and checks', /no problems found/i.test(await page.textContent('.check-result').catch(() => '')), await page.textContent('.check-result').catch(() => ''));

if (DATA) {
  const meta = fs.globSync(path.join(DATA, 'users', '*', 'notes', dst, 'meta.json'))[0];
  fs.rmSync(meta);
  await page.click('.check-actions button:has-text("Check my notes")');
  await page.waitForFunction(() => /Found/.test(document.querySelector('.check-result')?.textContent || ''), null, { timeout: 10000 }).catch(() => {});
  const found = await page.textContent('.check-result');
  check('a missing details file is found and named by its note', /Found one problem/.test(found) && found.includes(`Target ${tag}`) && !found.includes(DATA), found);
  check('…with a Repair button', await page.isVisible('.check-actions button:has-text("Repair")'));
  await page.click('.check-actions button:has-text("Repair")');
  await page.waitForFunction(() => /Repaired/.test(document.querySelector('.check-result')?.textContent || ''), null, { timeout: 10000 }).catch(() => {});
  check('Repair fixes it', /Repaired/.test(await page.textContent('.check-result')) && fs.existsSync(meta));
  check('the result is announced politely', (await page.getAttribute('.check-result', 'aria-live')) === 'polite');
}
await page.keyboard.press('Escape');
await settle();

// ---- export ----
await page.click('.user-btn');
await settle();
check('the account menu has "Export my notes"', await page.isVisible('[role="menuitem"]:has-text("Export my notes")'));
const [dl] = await Promise.all([page.waitForEvent('download', { timeout: 15000 }), page.click('[role="menuitem"]:has-text("Export my notes")')]);
const zip = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'exp-')), 'export.zip');
await dl.saveAs(zip);
const list = execFileSync('unzip', ['-Z1', zip]).toString().split('\n');
check('the export has one folder per note with note.txt and meta.json', list.includes(`notes/Target ${tag}/note.txt`) && list.includes(`notes/Target ${tag}/meta.json`), list.filter((l) => l.includes(tag)));
check('…and its images under their original names in files/', list.some((l) => l.startsWith(`notes/Target ${tag}/files/`) && l.endsWith('.png')));
check('…plus manifest.json for importing', list.includes('manifest.json'));
await page.keyboard.press('Control+k');
await page.keyboard.type('export my notes');
await settle();
check('the palette offers "Export my notes"', await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].some((o) => /export my notes/i.test(o.textContent))));
await page.keyboard.press('Escape');

await finish();
