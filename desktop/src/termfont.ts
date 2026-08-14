// Terminal font selection — pure model + tiny localStorage/event glue.
//
// The host PTY carries no font information (terminal protocol; mosaic behaves
// the same), so the on-screen font is decided entirely by the participant's
// xterm. D2Coding (SIL OFL) is bundled with the app and used by default because
// it is fixed-width for BOTH Latin and Hangul/CJK — so Korean and ASCII columns
// line up in the grid. The user may switch to another monospace font and size;
// non-bundled fonts only take effect when installed on the local machine, and
// bundled D2Coding is listed as a CJK fallback for them so Korean still renders.

export interface TermFont {
  family: string; // full CSS font-family stack applied to xterm + input overlay
  size: number; // px
}

export interface TermFontOption {
  label: string;
  family: string;
  note?: string; // "번들" for D2Coding, "설치 필요" for system-only fonts
}

// Ordered option list. Index 0 (D2Coding) is the default. Each non-bundled font
// keeps "D2Coding" in its stack so Hangul/CJK falls back to the bundled font.
export const TERM_FONTS: TermFontOption[] = [
  {
    label: "D2Coding",
    family: '"D2Coding", ui-monospace, SFMono-Regular, Menlo, monospace',
    note: "번들",
  },
  {
    label: "JetBrains Mono",
    family: '"JetBrains Mono", "D2Coding", ui-monospace, monospace',
    note: "설치 필요",
  },
  {
    label: "Menlo",
    family: 'Menlo, "D2Coding", ui-monospace, monospace',
  },
  {
    label: "SF Mono",
    family: '"SF Mono", "D2Coding", ui-monospace, monospace',
    note: "설치 필요",
  },
  {
    label: "Sarasa Term K",
    family: '"Sarasa Term K", "D2Coding", ui-monospace, monospace',
    note: "설치 필요",
  },
  {
    label: "Consolas",
    family: 'Consolas, "D2Coding", ui-monospace, monospace',
    note: "설치 필요",
  },
];

export const TERM_SIZES = [11, 12, 13, 14, 16] as const;

export const DEFAULT_TERM_FONT: TermFont = {
  family: TERM_FONTS[0].family,
  size: 13,
};

export const TERM_FONT_STORAGE_KEY = "pie-relay.term-font";
const LEGACY_TERM_FONT_STORAGE_KEY = "cli-relay.term-font";

// Window event name for live cross-room sync (see readTermFont/writeTermFont).
const TERM_FONT_EVENT = "pie-relay.term-font-change";

// Parse a persisted value into a valid TermFont. Pure so it is unit-testable:
// pass the raw localStorage string (or null). Unknown families and out-of-range
// sizes each fall back to the default independently, so a partially-stale value
// still yields a usable font. Any parse error yields the full default.
export function loadTermFont(raw: string | null): TermFont {
  if (!raw) return { ...DEFAULT_TERM_FONT };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ...DEFAULT_TERM_FONT };
  }
  if (!parsed || typeof parsed !== "object") return { ...DEFAULT_TERM_FONT };
  const obj = parsed as Record<string, unknown>;
  const family =
    typeof obj.family === "string" && TERM_FONTS.some((o) => o.family === obj.family)
      ? obj.family
      : DEFAULT_TERM_FONT.family;
  const size =
    typeof obj.size === "number" && (TERM_SIZES as readonly number[]).includes(obj.size)
      ? obj.size
      : DEFAULT_TERM_FONT.size;
  return { family, size };
}

export function serializeTermFont(font: TermFont): string {
  return JSON.stringify({ family: font.family, size: font.size });
}

// Read the current selection from localStorage (defaults if unset/invalid).
export function readTermFont(): TermFont {
  try {
    return loadTermFont(
      localStorage.getItem(TERM_FONT_STORAGE_KEY) ||
        localStorage.getItem(LEGACY_TERM_FONT_STORAGE_KEY),
    );
  } catch {
    return { ...DEFAULT_TERM_FONT };
  }
}

// Persist a selection and broadcast it so every mounted TerminalScreen (all
// rooms share one setting) applies it live.
export function writeTermFont(font: TermFont): void {
  try {
    localStorage.setItem(TERM_FONT_STORAGE_KEY, serializeTermFont(font));
  } catch {
    /* storage unavailable — still broadcast so this session applies it */
  }
  window.dispatchEvent(new CustomEvent<TermFont>(TERM_FONT_EVENT, { detail: font }));
}

// Subscribe to font changes; returns an unsubscribe function.
export function onTermFontChange(cb: (font: TermFont) => void): () => void {
  const handler = (e: Event) => cb((e as CustomEvent<TermFont>).detail);
  window.addEventListener(TERM_FONT_EVENT, handler);
  return () => window.removeEventListener(TERM_FONT_EVENT, handler);
}
