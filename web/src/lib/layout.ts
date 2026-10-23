/** Desktop layout preferences: collapsed panels and the writing column's width. */

export type PageWidth = 'auto' | 'normal' | 'wide' | 'full';
export const PAGE_WIDTHS: { id: PageWidth; label: string; hint: string }[] = [
  { id: 'auto', label: 'Auto', hint: 'Wider while the notes list is hidden' },
  { id: 'normal', label: 'Normal', hint: 'A comfortable reading column' },
  { id: 'wide', label: 'Wide', hint: 'More text per line' },
  { id: 'full', label: 'Full', hint: 'Use the whole window' },
];

export interface LayoutPrefs {
  sidebarCollapsed: boolean;
  listCollapsed: boolean;
  pageWidth: PageWidth;
}

type Store = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

const K = { sidebar: 'notes.sidebarCollapsed', list: 'notes.listCollapsed', width: 'notes.pageWidth', legacy: 'notes.sidebarHidden' };

export function loadLayout(s: Store): LayoutPrefs {
  // the old Ctrl+\ "hide sidebar" setting becomes the icon rail
  const legacy = s.getItem(K.legacy);
  if (legacy !== null) {
    if (s.getItem(K.sidebar) === null) s.setItem(K.sidebar, legacy === '1' ? '1' : '0');
    s.removeItem(K.legacy);
  }
  const w = s.getItem(K.width) as PageWidth | null;
  return {
    sidebarCollapsed: s.getItem(K.sidebar) === '1',
    listCollapsed: s.getItem(K.list) === '1',
    pageWidth: PAGE_WIDTHS.some((p) => p.id === w) ? w! : 'auto',
  };
}

export function saveLayout(s: Store, p: LayoutPrefs) {
  s.setItem(K.sidebar, p.sidebarCollapsed ? '1' : '0');
  s.setItem(K.list, p.listCollapsed ? '1' : '0');
  s.setItem(K.width, p.pageWidth);
}

/** The width actually used: Auto widens when the list is out of the way. */
export function effectiveWidth(p: PageWidth, listHidden: boolean): Exclude<PageWidth, 'auto'> {
  if (p !== 'auto') return p;
  return listHidden ? 'wide' : 'normal';
}
