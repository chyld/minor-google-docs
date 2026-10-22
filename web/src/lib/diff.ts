export interface DiffLine {
  kind: 'same' | 'mine' | 'theirs';
  text: string;
}

/** Line diff via LCS. Falls back to "all theirs, then all mine" for huge inputs. */
export function diffLines(mine: string, theirs: string): DiffLine[] {
  const a = theirs.split('\n');
  const b = mine.split('\n');
  if (a.length * b.length > 4_000_000) {
    return [...a.map((text) => ({ kind: 'theirs' as const, text })), ...b.map((text) => ({ kind: 'mine' as const, text }))];
  }
  const n = a.length;
  const m = b.length;
  const dp: Uint32Array[] = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--) dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      out.push({ kind: 'same', text: a[i] });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ kind: 'theirs', text: a[i++] });
    else out.push({ kind: 'mine', text: b[j++] });
  }
  while (i < n) out.push({ kind: 'theirs', text: a[i++] });
  while (j < m) out.push({ kind: 'mine', text: b[j++] });
  return out;
}
