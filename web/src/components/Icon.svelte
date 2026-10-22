<script lang="ts" module>
  // Hand-drawn 24px stroke icons (no icon font, no CDN).
  const P: Record<string, string> = {
    plus: 'M12 5v14M5 12h14',
    search: 'M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM20 20l-3.5-3.5',
    pin: 'M9 4h6l-1 5 3 3v2H7v-2l3-3-1-5zM12 14v6',
    trash: 'M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3',
    restore: 'M4 12a8 8 0 1 0 2.4-5.7L4 8.5M4 4v4.5h4.5',
    clip: 'M20 11.5l-7.8 7.8a5 5 0 0 1-7.1-7.1l8.5-8.5a3.3 3.3 0 0 1 4.7 4.7l-8.5 8.5a1.7 1.7 0 0 1-2.4-2.4l7.8-7.8',
    tag: 'M3 12V4a1 1 0 0 1 1-1h8l9 9-9 9-9-9zM8 8h.01',
    notes: 'M7 3h7l5 5v12a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM14 3v5h5M9 13h7M9 17h5',
    untag: 'M3 12V4a1 1 0 0 1 1-1h8l9 9-9 9-9-9zM3 3l18 18',
    files: 'M8 4h6l4 4v10a1 1 0 0 1-1 1H8a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zM4 8v12a1 1 0 0 0 1 1h9',
    image: 'M4 5h16v14H4zM4 16l5-5 5 5M13 14l2-2 5 5M15 9h.01',
    download: 'M12 4v11M7 10l5 5 5-5M5 20h14',
    x: 'M6 6l12 12M18 6L6 18',
    more: 'M5 12h.01M12 12h.01M19 12h.01',
    copy: 'M9 9h10v10H9zM5 15V5h10',
    back: 'M15 5l-7 7 7 7',
    menu: 'M4 7h16M4 12h16M4 17h16',
    sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4',
    moon: 'M20 14.5A8 8 0 0 1 9.5 4 8 8 0 1 0 20 14.5z',
    monitor: 'M3 5h18v11H3zM8 21h8M12 16v5',
    keyboard: 'M3 7h18v11H3zM7 11h.01M11 11h.01M15 11h.01M7 14h10',
    user: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21a8 8 0 0 1 16 0',
    logout: 'M15 4h4v16h-4M10 8l-4 4 4 4M6 12h10',
    check: 'M5 12l5 5 9-10',
    alert: 'M12 8v5M12 16h.01M10.3 3.9L2.5 18a2 2 0 0 0 1.7 3h15.6a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z',
    edit: 'M4 20h4L19 9l-4-4L4 16v4zM13 7l4 4',
    type: 'M5 6V4h14v2M12 4v16M9 20h6',
    sparkle: 'M12 3l1.8 5.2L19 10l-5.2 1.8L12 17l-1.8-5.2L5 10l5.2-1.8L12 3zM19 16l.8 2.2L22 19l-2.2.8L19 22l-.8-2.2L16 19l2.2-.8L19 16z',
    lock: 'M6 11h12v10H6zM8 11V8a4 4 0 0 1 8 0v3',
    focus: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
    sidebar: 'M3 4h18v16H3zM9 4v16',
    palette: 'M12 3a9 9 0 0 0 0 18c1.1 0 1.5-.8 1.5-1.5 0-1.2-1-1.3-1-2.5 0-.8.7-1.5 1.5-1.5H17a4 4 0 0 0 4-4c0-4.7-4-8.5-9-8.5zM7.5 11h.01M10 7.5h.01M14.5 7.5h.01',
    upload: 'M12 20V9M7 14l5-5 5 5M5 4h14',
    refresh: 'M20 12a8 8 0 1 1-2.3-5.7L20 8.5M20 4v4.5h-4.5',
    sort: 'M7 4v16M4 17l3 3 3-3M17 20V4M14 7l3-3 3 3',
    archive: 'M3 4h18v4H3zM5 8v12h14V8M10 12h4',
  };
</script>

<script lang="ts">
  interface Props {
    name: string;
    size?: number;
    stroke?: number;
    class?: string;
  }
  let { name, size = 18, stroke = 1.8, class: cls = '' }: Props = $props();
</script>

<svg
  class="icon {cls}"
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width={stroke}
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"><path d={P[name] ?? P.notes} /></svg>
