// Svelte actions for accessible overlays.

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])';

function focusables(node: HTMLElement): HTMLElement[] {
  return [...node.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => el.getClientRects().length > 0);
}

let openDialogs = 0;

function setInert(on: boolean) {
  document.querySelector('.shell')?.toggleAttribute('inert', on);
}

/** Moves the node to <body> (so an inert app shell doesn't swallow it). */
export function portal(node: HTMLElement) {
  document.body.appendChild(node);
  return { destroy: () => node.remove() };
}

/**
 * Modal dialog: focuses inside on open, traps Tab, closes on Escape from
 * anywhere, makes the app behind it inert, and restores focus on close.
 */
export function dialog(node: HTMLElement, opts: { onclose: () => void }) {
  let onclose = opts.onclose;
  const prev = document.activeElement as HTMLElement | null;
  openDialogs++;
  setInert(true);
  queueMicrotask(() => {
    if (node.contains(document.activeElement)) return;
    const target = node.querySelector<HTMLElement>('[data-autofocus], input:not([type="hidden"])') ?? focusables(node)[0] ?? node;
    target.focus();
  });
  const onkey = (e: KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      onclose();
    } else if (e.key === 'Tab') {
      const f = focusables(node);
      if (!f.length) return e.preventDefault();
      const first = f[0];
      const last = f[f.length - 1];
      const active = document.activeElement as HTMLElement | null;
      if (!active || !node.contains(active)) {
        e.preventDefault();
        first.focus();
      } else if (e.shiftKey && active === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    }
  };
  window.addEventListener('keydown', onkey, true);
  return {
    update(o: { onclose: () => void }) {
      onclose = o.onclose;
    },
    destroy() {
      window.removeEventListener('keydown', onkey, true);
      openDialogs = Math.max(0, openDialogs - 1);
      setInert(openDialogs > 0);
      if (prev && document.contains(prev)) prev.focus();
    },
  };
}

/** Dropdown menu: focus moves in, ↑/↓/Home/End move, Escape/Tab close and return focus. */
export function menu(node: HTMLElement, opts: { onclose: () => void }) {
  let onclose = opts.onclose;
  const trigger = document.activeElement as HTMLElement | null;
  const items = () => [...node.querySelectorAll<HTMLElement>('[role^="menuitem"]:not([disabled])')];
  queueMicrotask(() => (node.querySelector<HTMLElement>('[aria-checked="true"]') ?? items()[0] ?? node)?.focus());
  const close = (refocus: boolean) => {
    onclose();
    if (refocus && trigger && document.contains(trigger)) trigger.focus();
  };
  const onkey = (e: KeyboardEvent) => {
    const list = items();
    const i = list.indexOf(document.activeElement as HTMLElement);
    switch (e.key) {
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        close(true);
        return;
      case 'Tab':
        close(false);
        return;
      case 'ArrowDown':
        list[(i + 1) % list.length]?.focus();
        break;
      case 'ArrowUp':
        list[(i - 1 + list.length) % list.length]?.focus();
        break;
      case 'Home':
        list[0]?.focus();
        break;
      case 'End':
        list[list.length - 1]?.focus();
        break;
      default:
        return;
    }
    e.preventDefault();
  };
  window.addEventListener('keydown', onkey, true);
  return {
    update(o: { onclose: () => void }) {
      onclose = o.onclose;
    },
    destroy() {
      window.removeEventListener('keydown', onkey, true);
    },
  };
}
