import { describe, expect, it } from 'vitest';
import fixtureText from '../../../internal/core/testdata/sections.json?raw';
import { parseHeading, stripSections } from './heading';
import { parseLine, stripHighlights } from './highlight';

const fixture = JSON.parse(fixtureText);

describe('section headings (same fixture as the Go server)', () => {
  for (const c of fixture.headings as { line: string; color?: string; name?: string }[]) {
    it(JSON.stringify(c.line), () => {
      const h = parseHeading(c.line);
      if (!c.color) expect(h).toBeNull();
      else {
        expect(h).toMatchObject({ color: c.color, name: c.name });
        expect(c.line.slice(h!.prefix)).toBe(c.name);
      }
    });
  }
  for (const c of fixture.strip as { in: string; out: string }[]) {
    it(`strips ${JSON.stringify(c.in)}`, () => expect(stripSections(c.in)).toBe(c.out));
  }
  it('a heading line is never a highlight', () => {
    expect(parseLine('## {teal} =={amber}x==')).toEqual([]);
    expect(stripHighlights('## {teal} =={amber}x==')).toBe('## {teal} =={amber}x==');
  });
});

import css from '../app.css?raw';

describe('section heading colours are readable (WCAG AA, 4.5:1) in both themes', () => {
  const block = (sel: RegExp) => css.slice(css.search(sel)).split('}')[0];
  const tok = (b: string, name: string) => b.match(new RegExp(`--${name}:\\s*([^;]+);`))?.[1].trim() ?? '';
  const themes = { light: block(/:root,\s*\[data-theme='light'\]/), dark: block(/\[data-theme='dark'\]\s*\{/) };
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
  const hexLum = (hex: string) => {
    const v = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
    return 0.2126 * v[0] + 0.7152 * v[1] + 0.0722 * v[2];
  };
  const ratio = (a: number, b: number) => (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  for (const [theme, b] of Object.entries(themes)) {
    it(theme, () => {
      const l = +tok(b, 'sec-l');
      const c = +tok(b, 'sec-c');
      expect(l).toBeGreaterThan(0);
      for (const bg of ['bg', 'surface']) {
        const back = hexLum(tok(b, bg));
        for (const h of hues) expect(ratio(oklchLum(l, c, h), back), `${bg} hue ${h}`).toBeGreaterThanOrEqual(4.5);
      }
    });
  }
});
