(() => {
  'use strict';
  // Files are stored after this script as base64 chunks: <script type="application/octet-stream"
  // data-f="N">. They are looked up once the page has loaded, and decoded one chunk at a time,
  // never as one huge string.
  let chunks = null;
  const blocks = (f) => {
    if (!chunks) {
      chunks = {};
      for (const s of document.querySelectorAll('script[type="application/octet-stream"][data-f]')) {
        (chunks[s.dataset.f] = chunks[s.dataset.f] || []).push(s);
      }
    }
    return chunks[f];
  };
  const bytes = (b64) => {
    const bin = atob(b64);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  };
  const inlineSrc = (f) => {
    const img = document.querySelector('img[data-f="' + f + '"][src^="data:"]');
    return img ? img.getAttribute('src') : '';
  };
  const parts = (f) => {
    const b = blocks(f);
    if (b) return b.map((s) => bytes(s.textContent.trim()));
    const src = inlineSrc(f);
    return src ? [bytes(src.slice(src.indexOf(',') + 1))] : null;
  };
  const IMAGE = { 'image/png': 1, 'image/jpeg': 1, 'image/gif': 1, 'image/webp': 1 };
  const shown = {};
  // images kept as chunks (large ones, and repeats of small ones) get an object URL
  const fill = () => {
    for (const img of document.querySelectorAll('img[data-f]')) {
      if (img.getAttribute('src')) continue;
      const f = img.dataset.f;
      if (!shown[f]) {
        const small = inlineSrc(f);
        if (small) shown[f] = small;
        else {
          const p = parts(f);
          const type = IMAGE[img.dataset.type] ? img.dataset.type : 'application/octet-stream';
          shown[f] = p ? URL.createObjectURL(new Blob(p, { type })) : '';
        }
      }
      if (shown[f]) img.src = shown[f];
    }
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', fill);
  else fill();
  document.addEventListener('click', (e) => {
    const img = e.target.closest && e.target.closest('.img-frame img');
    if (img) img.classList.toggle('full');
    const btn = e.target.closest && e.target.closest('button[data-save]');
    if (!btn || btn.disabled) return;
    btn.disabled = true;
    setTimeout(() => {
      try {
        const p = parts(btn.dataset.save);
        if (!p) return;
        // always a download, never something the browser might open and run
        const url = URL.createObjectURL(new Blob(p, { type: 'application/octet-stream' }));
        const a = document.createElement('a');
        a.href = url;
        a.download = btn.dataset.name || 'file';
        document.body.append(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 60000);
      } finally {
        btn.disabled = false;
      }
    }, 0);
  });
  // dates in the reader's own time zone
  for (const t of document.querySelectorAll('time[datetime]')) {
    const d = new Date(t.getAttribute('datetime'));
    if (!isNaN(d)) t.textContent = d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
  }
  const ns = document.querySelector('.noscript');
  if (ns) ns.remove();
})();
