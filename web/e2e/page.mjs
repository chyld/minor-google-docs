// Browser checks for "Save as web page" (SPEC §23): the downloaded file opened
// from disk, offline, in Chromium and Firefox.
//   PW=... BASE=http://127.0.0.1:4500 USER_NAME=tester PASS=... node web/e2e/page.mjs
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { createRequire } from 'node:module';
import { browser, check, finish, makeNote, open, page } from './helpers.mjs';

const require = createRequire(import.meta.url);
const { firefox } = require(process.env.PW || 'playwright');
const settle = () => page.waitForTimeout(200);
const H = { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' };
const api = (method, url, body) =>
  page.evaluate(async ([method, url, body, H]) => {
    const r = await fetch(url, { method, headers: H, body: body ? JSON.stringify(body) : undefined });
    return r.status === 204 ? null : r.json();
  }, [method, url, body, H]);
const tag = Date.now().toString(36);
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'page-'));

// a PDF-like file (not an image) uploaded through the app's own upload protocol
async function uploadFile(noteId, name, bytesB64) {
  return page.evaluate(async ([noteId, name, b64]) => {
    const A = 'abcdefghijklmnopqrstuvwxyz234567';
    const ref = Array.from(crypto.getRandomValues(new Uint8Array(10)), (b) => A[b & 31]).join('');
    const bin = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    const id = (() => {
      const b = crypto.getRandomValues(new Uint8Array(16));
      const ts = Date.now();
      for (let i = 0; i < 6; i++) b[i] = Math.floor(ts / 2 ** (8 * (5 - i))) & 0xff;
      b[6] = (b[6] & 0x0f) | 0x70;
      b[8] = (b[8] & 0x3f) | 0x80;
      const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
      return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
    })();
    const e = (s) => btoa(unescape(encodeURIComponent(s)));
    const meta = [`id ${e(id)}`, `ref ${e(ref)}`, `note_id ${e(noteId)}`, `filename ${e(name)}`].join(',');
    const r = await fetch('/api/uploads/', {
      method: 'POST',
      headers: { 'X-Requested-With': 'notes', 'Tus-Resumable': '1.0.0', 'Upload-Length': String(bin.length), 'Upload-Metadata': meta, 'Content-Type': 'application/offset+octet-stream' },
      body: bin,
    });
    return { status: r.status, ref };
  }, [noteId, name, bytesB64]);
}

// ---- a note with everything in it ----
const pdf = Buffer.concat([Buffer.from('%PDF-1.4\n'), crypto.randomBytes(3 * 1024 * 1024)]); // > 1 chunk of nothing special
const id = await makeNote([`Trip ${tag}`, 'day one', { img: 140 }, '', '## {teal} Packing', 'boots', '', '']);
await page.evaluate(async ([id, H]) => {
  const n = await (await fetch(`/api/notes/${id}`, { headers: H })).json();
  const body = n.body.replace('day one', 'day =={amber}one== done') + '\n## {coral-deep} After\nlast line';
  await fetch(`/api/notes/${id}/body`, { method: 'PUT', headers: H, body: JSON.stringify({ body, base_sha256: n.sha256 }) });
}, [id, H]);
const up = await uploadFile(id, 'tickets.pdf', pdf.toString('base64'));
check('setup: a PDF attached', up.status === 201, up);
// a large photo (random pixels: over 2 MB as PNG), shown twice in the note
const noise = await page.evaluate(async () => {
  const c = document.createElement('canvas');
  c.width = c.height = 1100;
  const g = c.getContext('2d');
  const d = g.createImageData(1100, 1100);
  crypto.getRandomValues(d.data.subarray(0, 65536));
  for (let i = 0; i < d.data.length; i += 65536) crypto.getRandomValues(d.data.subarray(i, Math.min(i + 65536, d.data.length)));
  for (let i = 3; i < d.data.length; i += 4) d.data[i] = 255;
  g.putImageData(d, 0, 0);
  const blob = await new Promise((r) => c.toBlob(r, 'image/png'));
  const buf = new Uint8Array(await blob.arrayBuffer());
  let bin = '';
  for (let i = 0; i < buf.length; i += 32768) bin += String.fromCharCode(...buf.subarray(i, i + 32768));
  return btoa(bin);
});
const bigUp = await uploadFile(id, 'photo.png', noise);
check('setup: a large photo (> 2 MB) attached', bigUp.status === 201 && noise.length * 0.75 > 2 * 1024 * 1024, noise.length);
await page.evaluate(async ([id, H, ref]) => {
  const n = await (await fetch(`/api/notes/${id}`, { headers: H })).json();
  const body = n.body + `\n[image:${ref}] photo\n[image:${ref}]`;
  await fetch(`/api/notes/${id}/body`, { method: 'PUT', headers: H, body: JSON.stringify({ body, base_sha256: n.sha256 }) });
}, [id, H, bigUp.ref]);
await open(id);

async function download(trigger) {
  const [dl] = await Promise.all([page.waitForEvent('download', { timeout: 20000 }), trigger()]);
  const file = path.join(dir, `${Date.now()}-${dl.suggestedFilename()}`);
  await dl.saveAs(file);
  return { file, name: dl.suggestedFilename() };
}

// ---- from the ⋯ menu ----
await page.click('button[aria-label="More actions"]');
await settle();
check('the ⋯ menu has "Save as web page"', await page.isVisible('[role="menuitem"]:has-text("Save as web page")'));
const got = await download(() => page.click('[role="menuitem"]:has-text("Save as web page")'));
check('it downloads "<title>.html"', got.name === `Trip ${tag}.html`, got.name);
const html = fs.readFileSync(got.file, 'utf8');
check('the file is one self-contained page (nothing loaded from anywhere)', !/(src|href)="https?:/.test(html) && html.includes('<meta http-equiv="Content-Security-Policy"'));

async function inspect(br, label) {
  // Playwright's "offline" mode also blocks blob: downloads (a real offline browser doesn't),
  // so instead every web request is recorded and refused: the page must make none
  const ctx = await br.newContext({ acceptDownloads: true });
  const requests = [];
  await ctx.route(/^(https?|wss?):/, (r) => (requests.push(r.request().url()), r.abort()));
  const p = await ctx.newPage();
  const errors = [];
  p.on('pageerror', (e) => errors.push(e.message));
  p.on('console', (m) => m.type() === 'error' && errors.push(m.text()));
  await p.goto('file://' + got.file);
  await p.waitForTimeout(500);
  const r = await p.evaluate(() => {
    const cs = (sel) => (document.querySelector(sel) ? getComputedStyle(document.querySelector(sel)) : null);
    const imgs = [...document.querySelectorAll('.img-frame img')];
    return {
      title: document.querySelector('.title')?.textContent,
      font: cs('body').fontFamily,
      imgs: imgs.length && imgs.every((i) => i.complete && i.naturalWidth > 0),
      count: imgs.length,
      hl: cs('.hl')?.backgroundColor,
      secColor: cs('.sec-head')?.color,
      ink: cs('body').color,
      bar: document.querySelector('.sec-body') ? getComputedStyle(document.querySelector('.sec-body'), '::before').width : '',
      blank: (() => {
        // a run ending in blank lines is taller than one line of text
        const runs = [...document.querySelectorAll('.text')];
        const one = runs.find((r) => r.textContent === 'boots');
        return one ? one.getBoundingClientRect().height : 0;
      })(),
      end: !!document.querySelector('.note-end'),
      files: [...document.querySelectorAll('.file .name')].map((n) => n.textContent),
      noscript: !!document.querySelector('.noscript'),
      fontLoaded: document.fonts.check('16px "Inter Variable"'),
    };
  });
  check(`${label}: the page shows the note's title`, r.title === `Trip ${tag}`, r.title);
  check(`${label}: images decode offline (incl. the large photo, twice)`, r.imgs && r.count === 3, r.count);
  check(`${label}: highlights and section colours are drawn`, !!r.hl && r.hl !== 'rgba(0, 0, 0, 0)' && r.secColor !== r.ink, r);
  check(`${label}: the section bar is there (3px)`, r.bar === '3px', r.bar);
  check(`${label}: the end mark and the file list`, r.end && r.files.includes('tickets.pdf'), r.files);
  check(`${label}: the script ran (the "needs JavaScript" line is gone)`, !r.noscript);
  check(`${label}: the app's font is embedded and used`, r.fontLoaded && r.font.includes('Inter'), r.font);
  check(`${label}: no errors (CSP lets the one script run)`, errors.length === 0, errors);
  check(`${label}: the page asks the network for nothing`, requests.length === 0, requests);
  // Save a file: the exact bytes come back
  const [dl] = await Promise.all([p.waitForEvent('download', { timeout: 20000 }), p.click('button[aria-label="Save tickets.pdf"]')]);
  const out = path.join(dir, `${label}-tickets.pdf`);
  await dl.saveAs(out);
  {
    const [d2] = await Promise.all([p.waitForEvent('download', { timeout: 20000 }), p.click('button[aria-label="Save photo.png"]')]);
    const o2 = path.join(dir, `${label}-photo.png`);
    await d2.saveAs(o2);
    check(`${label}: the large photo saves back identical too`, Buffer.compare(fs.readFileSync(o2), Buffer.from(noise, 'base64')) === 0);
  }
  check(`${label}: Save gives back the identical file`, dl.suggestedFilename() === 'tickets.pdf' && Buffer.compare(fs.readFileSync(out), pdf) === 0, dl.suggestedFilename());
  // print: light colours, no buttons
  await p.emulateMedia({ media: 'print', colorScheme: 'dark' });
  const pr = await p.evaluate(() => ({ bg: getComputedStyle(document.body).backgroundColor, btn: getComputedStyle(document.querySelector('.file button')).display }));
  check(`${label}: printing is light and has no buttons`, pr.bg === 'rgb(255, 255, 255)' && pr.btn === 'none', pr);
  await ctx.close();
}

await inspect(browser, 'chromium');
let ff;
try {
  ff = await firefox.launch();
} catch {
  /* not installed */
}
if (ff) {
  await inspect(ff, 'firefox');
  await ff.close();
} else {
  check('firefox available for the cross-browser check', false);
}

// scripts blocked: text and small images still show, with a note about saving files
{
  const ctx = await browser.newContext({ javaScriptEnabled: false });
  const p = await ctx.newPage();
  await p.goto('file://' + got.file);
  const r = await p.evaluate(() => ({
    img: [...document.querySelectorAll('.img-frame img')].some((i) => i.complete && i.naturalWidth > 0),
    note: !!document.querySelector('.noscript'),
  })).catch(() => null);
  // evaluate needs JS in the page context; Playwright's own evaluate still works with page JS off
  check('without JavaScript the text and small images show, and it says saving needs JavaScript', !!r && r.img && r.note, r);
  await ctx.close();
}

// ---- the theme follows the app ----
await page.evaluate(() => localStorage.setItem('notes.theme', 'dark'));
await open(id);
await page.keyboard.press('Control+k');
await page.keyboard.type('save note as web page');
await settle();
check('the palette offers "Save note as web page"', await page.evaluate(() => [...document.querySelectorAll('[role="option"]')].some((o) => /save note as web page/i.test(o.textContent))));
const dark = await download(() => page.keyboard.press('Enter'));
check('the page follows the app\'s dark theme', fs.readFileSync(dark.file, 'utf8').includes('data-theme="dark"'));
await page.evaluate(() => localStorage.removeItem('notes.theme'));

// ---- very large pages ask first ----
await open(id);
await page.route('**/html?estimate=1**', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ bytes: 700 * 1024 * 1024 }) }));
let asked = '';
page.once('dialog', (d) => ((asked = d.message()), d.dismiss()));
let downloaded = false;
page.once('download', () => (downloaded = true));
await page.click('button[aria-label="More actions"]');
await page.click('[role="menuitem"]:has-text("Save as web page")');
await page.waitForTimeout(800);
check('over 100 MB it asks first (and Cancel downloads nothing)', /about 700 MB/.test(asked) && !downloaded, asked);
await page.unroute('**/html?estimate=1**');

// ---- trashed notes: the button sits beside Restore ----
await api('PATCH', `/api/notes/${id}`, { trashed: true });
await open(id);
check('a trashed note has the button next to Restore', await page.isVisible('button[aria-label="Save as web page"]'));
const trashed = await download(() => page.click('button[aria-label="Save as web page"]'));
check('…and its page says it is in the trash', fs.readFileSync(trashed.file, 'utf8').includes('In the trash'));
await api('PATCH', `/api/notes/${id}`, { trashed: false });

await finish();
