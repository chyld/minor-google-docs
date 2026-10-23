// Shared setup for the browser suites in this folder.
// Playwright is not a project dependency; point PW at any install.
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PW || 'playwright');
export const BASE = process.env.BASE || 'http://127.0.0.1:4500';
const USER = process.env.USER_NAME || process.env.USER || 'tester';
const PASS = process.env.PASS || 'testpass-123';

const results = [];
export function check(name, ok, detail = '') {
  results.push({ name, ok });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok || !detail ? '' : `  — ${JSON.stringify(detail)}`}`);
}

export const browser = await chromium.launch();
export const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
await page.goto(BASE + '/');
const login = await page.request.post(BASE + '/api/login', {
  headers: { 'X-Requested-With': 'notes' },
  data: { username: USER, password: PASS },
});
if (!login.ok()) throw new Error('login failed: ' + login.status());

// ---- helpers running in the page ----

/** Create a note whose body lines are text or {img: height}; uploads real PNGs for the images. */
export async function makeNote(lines) {
  return page.evaluate(async (lines) => {
    const H = { 'X-Requested-With': 'notes', 'Content-Type': 'application/json' };
    const A = 'abcdefghijklmnopqrstuvwxyz234567';
    const ref = () => Array.from(crypto.getRandomValues(new Uint8Array(10)), (b) => A[b & 31]).join('');
    const imgs = [];
    const body = lines
      .map((l, i) => {
        if (typeof l === 'string') return l;
        const r = ref();
        imgs.push({ r, h: l.img, i });
        return `[image:${r}] shot${i}.png`;
      })
      .join('\n');
    const n = await (await fetch('/api/notes', { method: 'POST', headers: H, body: JSON.stringify({ body }) })).json();
    const id = n.id ?? n.note.id;
    const b64 = (s) => btoa(unescape(encodeURIComponent(s)));
    const uuidv7 = () => {
      const b = crypto.getRandomValues(new Uint8Array(16));
      const ts = Date.now();
      for (let i = 0; i < 6; i++) b[i] = Math.floor(ts / 2 ** (8 * (5 - i))) & 0xff;
      b[6] = (b[6] & 0x0f) | 0x70;
      b[8] = (b[8] & 0x3f) | 0x80;
      const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
      return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
    };
    for (const { r, h, i } of imgs) {
      const c = document.createElement('canvas');
      c.width = 900;
      c.height = h;
      const g = c.getContext('2d');
      g.fillStyle = `hsl(${i * 70} 60% 45%)`;
      g.fillRect(0, 0, 900, h);
      const blob = await new Promise((res) => c.toBlob(res, 'image/png'));
      const meta = [`id ${b64(uuidv7())}`, `ref ${b64(r)}`, `note_id ${b64(id)}`, `filename ${b64(`shot${i}.png`)}`].join(',');
      const res = await fetch('/api/uploads/', {
        method: 'POST',
        headers: { 'X-Requested-With': 'notes', 'Tus-Resumable': '1.0.0', 'Upload-Length': String(blob.size), 'Upload-Metadata': meta, 'Content-Type': 'application/offset+octet-stream' },
        body: blob,
      });
      if (res.status !== 201) throw new Error('upload ' + res.status);
    }
    return id;
  }, lines);
}

export async function open(id) {
  await page.goto(`${BASE}/n/${id}`);
  await page.waitForSelector('.cm-content', { state: 'attached' });
  await page.evaluate(async () => {
    let o = document.querySelector('.cm-content').cmTile;
    while (o && !o.view) o = o.parent;
    window.__v = o.view;
    // wait for every image to load, then a frame for measuring
    for (let i = 0; i < 100; i++) {
      const imgs = [...document.querySelectorAll('.cm-img img')];
      if (imgs.length && imgs.every((im) => im.complete && im.naturalHeight)) break;
      await new Promise((r) => setTimeout(r, 50));
    }
    await new Promise((r) => setTimeout(r, 300));
  });
}


export async function finish() {
  await browser.close();
  const failed = results.filter((r) => !r.ok).length;
  console.log(`\n${results.length - failed}/${results.length} passed`);
  process.exit(failed ? 1 : 0);
}
