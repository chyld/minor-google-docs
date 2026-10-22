/** RFC 9562 UUIDv7: 48-bit ms timestamp + random. */
export function uuidv7(): string {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  const ts = Date.now();
  b[0] = Math.floor(ts / 2 ** 40) & 0xff;
  b[1] = Math.floor(ts / 2 ** 32) & 0xff;
  b[2] = (ts >>> 24) & 0xff;
  b[3] = (ts >>> 16) & 0xff;
  b[4] = (ts >>> 8) & 0xff;
  b[5] = ts & 0xff;
  b[6] = (b[6] & 0x0f) | 0x70;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

const ALPHABET = 'abcdefghijklmnopqrstuvwxyz234567';

/** 10-char base32 marker ref (50 random bits). */
export function newRef(): string {
  const b = new Uint8Array(10);
  crypto.getRandomValues(b);
  return [...b].map((x) => ALPHABET[x & 31]).join('');
}

export const MARKER_RE = /^\[image:([a-z2-7]{10})\](?: (.*))?$/;

export function markerLine(ref: string, filename: string): string {
  const caption = filename.replace(/[\r\n]+/g, ' ').trim();
  return caption ? `[image:${ref}] ${caption}` : `[image:${ref}]`;
}
