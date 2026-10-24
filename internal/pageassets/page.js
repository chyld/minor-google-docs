(() => {
  'use strict';
  // Files are stored after this script as base64 chunks: script elements of type
  // application/octet-stream with data-f="N". They are found once the page has loaded,
  // decoded one chunk at a time into a Blob (never one huge string), and the base64
  // text is then dropped. A Blob is made once per file and reused.
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
  const blobs = {};
  const blob = (f) => {
    if (blobs[f]) return blobs[f];
    const b = blocks(f);
    let parts = null;
    if (b) {
      parts = [];
      for (const s of b) {
        parts.push(bytes(s.textContent.trim()));
        s.remove(); // the base64 text isn't needed any more
      }
    } else {
      const src = inlineSrc(f);
      if (src) parts = [bytes(src.slice(src.indexOf(',') + 1))];
    }
    return parts ? (blobs[f] = new Blob(parts, { type: 'application/octet-stream' })) : null;
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
          try {
            const b = blob(f);
            const type = IMAGE[img.dataset.type] ? img.dataset.type : 'application/octet-stream';
            shown[f] = b ? URL.createObjectURL(new Blob([b], { type })) : '';
          } catch (e) {
            shown[f] = '';
          }
        }
      }
      if (shown[f]) img.src = shown[f];
    }
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', fill);
  else fill();
  const zoom = (img) => img.classList.toggle('full');
  document.addEventListener('keydown', (e) => {
    if ((e.key === 'Enter' || e.key === ' ') && e.target.matches && e.target.matches('.img-frame img')) {
      e.preventDefault();
      zoom(e.target);
    }
  });
  document.addEventListener('click', (e) => {
    const img = e.target.closest && e.target.closest('.img-frame img');
    if (img) zoom(img);
    const btn = e.target.closest && e.target.closest('button[data-save]');
    if (!btn || btn.disabled) return;
    btn.disabled = true;
    setTimeout(() => {
      try {
        const b = blob(btn.dataset.save);
        if (!b) throw new Error('missing');
        // always a download, never something the browser might open and run
        const url = URL.createObjectURL(b);
        const a = document.createElement('a');
        a.href = url;
        a.download = btn.dataset.name || 'file';
        document.body.append(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 60000);
      } catch (e) {
        btn.textContent = "Can't read this file";
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
