import { describe, expect, it } from 'vitest';
import { effectiveWidth, loadLayout, saveLayout } from './layout';

function store(init: Record<string, string> = {}) {
  const m = new Map(Object.entries(init));
  return {
    m,
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
    removeItem: (k: string) => void m.delete(k),
  };
}

describe('layout preferences', () => {
  it('defaults to everything open and Auto width', () => {
    expect(loadLayout(store())).toEqual({ sidebarCollapsed: false, listCollapsed: false, pageWidth: 'auto' });
  });

  it('turns the old hidden-sidebar setting into the icon rail, once', () => {
    const s = store({ 'notes.sidebarHidden': '1' });
    expect(loadLayout(s).sidebarCollapsed).toBe(true);
    expect(s.m.has('notes.sidebarHidden')).toBe(false);
    expect(loadLayout(s).sidebarCollapsed).toBe(true);
  });

  it('a newer setting wins over the old one', () => {
    const s = store({ 'notes.sidebarHidden': '1', 'notes.sidebarCollapsed': '0' });
    expect(loadLayout(s).sidebarCollapsed).toBe(false);
  });

  it('round-trips and ignores junk widths', () => {
    const s = store();
    saveLayout(s, { sidebarCollapsed: true, listCollapsed: true, pageWidth: 'full' });
    expect(loadLayout(s)).toEqual({ sidebarCollapsed: true, listCollapsed: true, pageWidth: 'full' });
    s.setItem('notes.pageWidth', 'huge');
    expect(loadLayout(s).pageWidth).toBe('auto');
  });

  it('Auto widens only while the list is hidden', () => {
    expect(effectiveWidth('auto', false)).toBe('normal');
    expect(effectiveWidth('auto', true)).toBe('wide');
    expect(effectiveWidth('normal', true)).toBe('normal');
    expect(effectiveWidth('full', false)).toBe('full');
  });
});
