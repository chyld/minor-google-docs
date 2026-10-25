// Browser checks for "Download as .tar.gz" (SPEC §24): the archive downloaded from the
// ⋯ menu and the palette, unpacked with the system's tar.
//   PW=... BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/archive.mjs
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { check, finish, makeNote, open, page } from './helpers.mjs';

const H = { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' };
const api = (method, url, body) =>
  page.evaluate(async ([method, url, body, H]) => {
    const r = await fetch(url, { method, headers: H, body: body ? JSON.stringify(body) : undefined });
    return r.status === 204 ? null : r.json();
  }, [method, url, body, H]);
const tag = Date.now().toString(36);
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'archive-'));

async function download(trigger) {
  const [dl] = await Promise.all([page.waitForEvent('download', { timeout: 20000 }), trigger()]);
  const file = path.join(dir, `${Date.now()}-${dl.suggestedFilename()}`);
  await dl.saveAs(file);
  return { file, name: dl.suggestedFilename() };
}
const unpack = (file) => {
  const out = fs.mkdtempSync(path.join(dir, 'x-'));
  execFileSync('tar', ['-xzf', file, '-C', out]);
  return out;
};
const toastText = () => page.evaluate(() => [...document.querySelectorAll('.toast, [role="status"]')].map((e) => e.textContent).join(' | '));

// ---- a note with images, a missing one, and the ⋯ menu ----
const id = await makeNote([`Rack ${tag}`, 'front', { img: 120 }, 'back', { img: 90 }]);
await page.evaluate(async ([id, H]) => {
  const n = await (await fetch(`/api/notes/${id}`, { headers: H })).json();
  await fetch(`/api/notes/${id}/body`, { method: 'PUT', headers: H, body: JSON.stringify({ body: n.body + '\n[image:aaaaaaaaaa] lost photo', base_sha256: n.sha256 }) });
}, [id, H]);
await open(id);
await page.click('button[aria-label="More actions"]');
check('the ⋯ menu has "Download as .tar.gz"', await page.isVisible('[role="menuitem"]:has-text("Download as .tar.gz")'));
const got = await download(() => page.click('[role="menuitem"]:has-text("Download as .tar.gz")'));
check('it downloads "<title>.tar.gz"', got.name === `Rack ${tag}.tar.gz`, got.name);
const out = unpack(got.file);
const folder = path.join(out, 'notes', `Rack ${tag}`);
const files = fs.existsSync(path.join(folder, 'files')) ? fs.readdirSync(path.join(folder, 'files')).sort() : [];
check('it unpacks to notes/<title>/ with note.txt, meta.json and the images', fs.existsSync(path.join(folder, 'note.txt')) && fs.existsSync(path.join(folder, 'meta.json')) && files.join() === 'shot2.png,shot4.png', files);
const man = JSON.parse(fs.readFileSync(path.join(out, 'manifest.json'), 'utf8'));
check('manifest.json lists the note and its two images', man.version === 2 && man.notes.length === 1 && man.notes[0].attachments.length === 2, man.notes[0]);
check('MISSING.txt names the image that exists nowhere', /lost photo/.test(fs.readFileSync(path.join(out, 'MISSING.txt'), 'utf8')));
check('…and a toast says so', /1 file is missing on the server/i.test(await toastText()), await toastText());
const note = await api('GET', `/api/notes/${id}`);
check('note.txt is the note as saved', fs.readFileSync(path.join(folder, 'note.txt'), 'utf8') === note.body);

// ---- the palette ----
await page.keyboard.press('Control+k');
await page.keyboard.type('tar.gz');
await page.waitForTimeout(200);
const viaPalette = await download(() => page.keyboard.press('Enter'));
check('the palette downloads it too', viaPalette.name === `Rack ${tag}.tar.gz`, viaPalette.name);

// ---- refusals ----
{
  const n = await makeNote([`Busy ${tag}`, 'text']);
  await open(n);
  await page.route('**/api/notes/*/body', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":{"code":"internal","message":"x"}}' }));
  await page.evaluate(() => __v.dispatch({ changes: { from: __v.state.doc.length, insert: ' more' } }));
  let dl = false;
  page.once('download', () => (dl = true));
  await page.click('button[aria-label="More actions"]');
  await page.click('[role="menuitem"]:has-text("Download as .tar.gz")');
  await page.waitForTimeout(1500);
  check('with unsaved changes it refuses and says why (no download)', !dl && /the archive is made from the saved note/i.test(await page.content()), await toastText());
  await page.unroute('**/api/notes/*/body');
  await page.evaluate(() => window.dispatchEvent(new CustomEvent('notes:save')));
  await page.waitForTimeout(1200);
}
await api('PATCH', `/api/notes/${id}`, { trashed: true });
await open(id);
check('a trashed note has no ⋯ menu, so no "Download as .tar.gz"', !(await page.isVisible('button[aria-label="More actions"]')) && !(await page.isVisible('text=Download as .tar.gz')));
await page.keyboard.press('Control+k');
await page.keyboard.type('tar.gz');
await page.waitForTimeout(200);
check('…nor a palette entry', !(await page.isVisible('text=Download note as .tar.gz')));
await page.keyboard.press('Escape');
const res = await page.evaluate(async (id) => (await fetch(`/api/notes/${id}/archive`, { headers: { 'X-Requested-With': 'notes' } })).status, id);
check('…and the server refuses it (409)', res === 409, res);
await api('PATCH', `/api/notes/${id}`, { trashed: false });

await finish();
