import { describe, expect, it } from 'vitest';
import fixtureText from '../../../internal/core/testdata/highlights.json?raw';
import css from '../app.css?raw';
import { HL_NAMES, lineRuns, parseLine, pieces, recolor, serialize, stripHighlights } from './highlight';

const fixture = JSON.parse(fixtureText);

describe('the shared grammar (same fixture as the Go server)', () => {
  it('has the same 24 names', () => {
    expect(HL_NAMES).toEqual(fixture.names);
  });
  for (const c of fixture.strip as { in: string; out: string }[]) {
    it(`strips ${JSON.stringify(c.in)}`, () => expect(stripHighlights(c.in)).toBe(c.out));
  }
});

describe('writing highlights back', () => {
  const colour = (s: string, name = 'coral') => Array(s.length).fill(name);

  it('never writes "==" inside a highlight or "=" at its end', () => {
    expect(serialize('a == b', colour('a == b')).text).toBe('=={coral}a ======{coral} b==');
    expect(parseLine('=={coral}a ======{coral} b==').map((h) => h.name)).toEqual(['coral', 'coral']);
    expect(serialize('x=', colour('x=')).text).toBe('=={coral}x===');
    expect(stripHighlights(serialize('a == b', colour('a == b')).text)).toBe('a == b');
  });

  it('leaves whitespace-only stretches plain', () => {
    expect(pieces('   ', colour('   '))).toEqual([]);
  });

  it('round-trips: parse → write gives the same text, and every write strips back to the plain text', () => {
    const chars = ['a', 'b', ' ', '=', '{', '}', 'é'];
    let seed = 7;
    const rnd = (n: number) => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) % n);
    for (let k = 0; k < 2000; k++) {
      const plain = Array.from({ length: 1 + rnd(12) }, () => chars[rnd(chars.length)]).join('');
      const colors = Array.from(plain, () => (rnd(3) ? null : HL_NAMES[rnd(4)]));
      const out = serialize(plain, colors).text;
      expect(stripHighlights(out)).toBe(plain);
      const runs = lineRuns(out);
      expect(runs.plain).toBe(plain);
      expect(serialize(runs.plain, runs.colors).text).toBe(out);
    }
  });

  it('recolouring part of a highlight splits it; the same colour merges', () => {
    const runs = lineRuns('=={coral}hello world==');
    const split = recolor(runs, 6, 11, 'sky')!;
    expect(split.text).toBe('=={coral}hello ===={sky}world==');
    const merged = recolor(lineRuns(split.text), 6, 11, 'coral')!;
    expect(merged.text).toBe('=={coral}hello world==');
  });

  it('positions map to the inside of a highlight', () => {
    const r = serialize('ab', colour('ab'));
    expect(r.after[0]).toBe(9); // after the opener
    expect(r.before[2]).toBe(11); // before the closer
  });

  it('image lines are never highlights', () => {
    expect(parseLine('[image:abcdefghij] =={coral}x==')).toEqual([]);
  });
});

describe('colours are readable (WCAG AA, 4.5:1) in both themes', () => {
  const block = (sel: RegExp) => css.slice(css.search(sel)).split('}')[0];
  const tokens = (b: string) => Object.fromEntries([...b.matchAll(/--(hl-[\w-]+):\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));
  const themes = { light: tokens(block(/:root,\s*\[data-theme='light'\]/)), dark: tokens(block(/\[data-theme='dark'\]\s*\{/)) };
  const hues = [25, 50, 78, 125, 150, 170, 195, 228, 262, 295, 328, 355];

  const lin = (c: number) => Math.min(1, Math.max(0, c));
  function oklchLum(l: number, c: number, h: number) {
    const a = c * Math.cos((h * Math.PI) / 180);
    const b = c * Math.sin((h * Math.PI) / 180);
    const l_ = (l + 0.3963377774 * a + 0.2158037573 * b) ** 3;
    const m_ = (l - 0.1055613458 * a - 0.0638541728 * b) ** 3;
    const s_ = (l - 0.0894841775 * a - 1.291485548 * b) ** 3;
    const r = lin(4.0767416621 * l_ - 3.3077115913 * m_ + 0.2309699292 * s_);
    const g = lin(-1.2684380046 * l_ + 2.6097574011 * m_ - 0.3413193965 * s_);
    const bl = lin(-0.0041960863 * l_ - 0.7034186147 * m_ + 1.707614701 * s_);
    return 0.2126 * r + 0.7152 * g + 0.0722 * bl;
  }
  function hexLum(hex: string) {
    const v = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
    return 0.2126 * v[0] + 0.7152 * v[1] + 0.0722 * v[2];
  }
  const ratio = (a: number, b: number) => (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);

  for (const [theme, t] of Object.entries(themes)) {
    for (const v of ['hl', 'hl-deep']) {
      it(`${theme} ${v}`, () => {
        const ink = hexLum(t[`${v}-ink`]);
        for (const h of hues) {
          const r = ratio(ink, oklchLum(+t[`${v}-l`], +t[`${v}-c`], h));
          expect(r, `hue ${h}`).toBeGreaterThanOrEqual(4.5);
        }
      });
    }
  }
});
