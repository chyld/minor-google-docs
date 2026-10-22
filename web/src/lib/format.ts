const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

export function relativeTime(ms: number, now = Date.now()): string {
  const s = Math.round((ms - now) / 1000);
  const abs = Math.abs(s);
  if (abs < 45) return 'just now';
  if (abs < 3600) return rtf.format(Math.round(s / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(s / 3600), 'hour');
  if (abs < 86400 * 7) return rtf.format(Math.round(s / 86400), 'day');
  const d = new Date(ms);
  const sameYear = d.getFullYear() === new Date(now).getFullYear();
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', ...(sameYear ? {} : { year: 'numeric' }) });
}

export function fullTime(ms: number): string {
  return new Date(ms).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

export function bytes(n: number): string {
  if (n < 0) return '—';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

export function wordCount(s: string): number {
  const m = s.match(/\S+/g);
  return m ? m.length : 0;
}

export function stamp(d = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** File-type family used for icon + color in the attachment panel. */
export function fileKind(mime: string, name: string): { kind: string; hue: number } {
  const ext = name.split('.').pop()?.toLowerCase() ?? '';
  if (mime.startsWith('image/')) return { kind: 'image', hue: 330 };
  if (mime.startsWith('video/')) return { kind: 'video', hue: 295 };
  if (mime.startsWith('audio/')) return { kind: 'audio', hue: 260 };
  if (mime === 'application/pdf' || ext === 'pdf') return { kind: 'pdf', hue: 25 };
  if (['zip', 'gz', 'tgz', 'xz', 'zst', '7z', 'rar', 'tar', 'bz2', 'iso'].includes(ext)) return { kind: 'archive', hue: 80 };
  if (['js', 'ts', 'go', 'py', 'rs', 'sh', 'json', 'yaml', 'yml', 'toml', 'conf', 'ini', 'xml', 'html', 'css', 'sql', 'c', 'h', 'cpp', 'java'].includes(ext))
    return { kind: 'code', hue: 195 };
  if (mime.startsWith('text/') || ['txt', 'md', 'log', 'csv'].includes(ext)) return { kind: 'text', hue: 230 };
  if (['doc', 'docx', 'odt', 'xls', 'xlsx', 'ods', 'ppt', 'pptx', 'odp'].includes(ext)) return { kind: 'doc', hue: 150 };
  return { kind: 'file', hue: 55 };
}
