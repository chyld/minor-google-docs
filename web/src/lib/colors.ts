/** Tag palette: each key maps to an OKLCH hue; lightness/chroma come from CSS tokens. */
export const HUES: Record<string, number> = {
  coral: 25,
  orange: 50,
  amber: 78,
  lime: 125,
  green: 150,
  mint: 170,
  teal: 195,
  sky: 228,
  blue: 262,
  violet: 295,
  magenta: 328,
  pink: 355,
};

export const PALETTE = Object.keys(HUES);

export const hue = (color: string) => HUES[color] ?? 262;
