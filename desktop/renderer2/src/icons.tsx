// SVG icons — paths lifted verbatim from the approved mock (workass-one.html).
import type { ReactNode } from 'react';
import type { ActionIcon } from './tool-names';

function S({ children, width }: { children: ReactNode; width?: number }) {
  return <svg width={width} viewBox="0 0 16 16" fill="none" stroke="currentColor">{children}</svg>;
}

export const IcSidebar = () => <S><rect x="2" y="3" width="12" height="10" rx="2" /><path d="M6 3v10" /></S>;
export const IcSearch = () => <S><circle cx="7" cy="7" r="4.5" /><path d="M10.5 10.5L14 14" /></S>;
export const IcAssist = () => <S><path d="M2.5 8L8 3l5.5 5M4 7v6h8V7" /></S>;
export const IcChats = () => <S><path d="M5.5 5L2.5 8l3 3M10.5 5l3 3-3 3" /></S>;
export const IcPlus = () => <S><path d="M8 3v10M3 8h10" /></S>;
export const IcActivity = () => <S><path d="M2 8h2.5l2-4 3 8 2-4H14" /></S>;
export const IcGear = () => <S><circle cx="8" cy="8" r="2.4" /><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.3 3.3l1.4 1.4M11.3 11.3l1.4 1.4M12.7 3.3l-1.4 1.4M4.7 11.3l-1.4 1.4" /></S>;
export const IcDoc = () => <S width={14}><rect x="2" y="4" width="12" height="8" rx="1.5" /><path d="M5 14h6" /></S>;
export const IcTerminal = () => <S><path d="M3 4.5l3.5 3.5L3 11.5" /><path d="M8.5 12h4.5" /></S>;
export const IcChanges = () => <S><rect x="2.5" y="2.5" width="11" height="11" rx="2" /><path d="M8 5.5v5M5.5 8h5" /></S>;
export const IcPreview = () => <S><path d="M5 3l8 5-8 5z" /></S>;
export const IcRail = () => <S><rect x="2" y="3" width="12" height="10" rx="2" /><path d="M10 3v10" /></S>;
export const IcBrowser = () => <S><circle cx="8" cy="8" r="6" /><path d="M2 8h12M8 2c1.7 1.6 2.7 3.7 2.7 6S9.7 12.4 8 14c-1.7-1.6-2.7-3.7-2.7-6S6.3 3.6 8 2z" /></S>;
export const IcEdit = () => <S><path d="M11 2l3 3-8.5 8.5H2.5V10.5z" /></S>;
export const IcShield = () => <S><path d="M8 2l5 2.5v4C13 11.5 10.8 13.4 8 14 5.2 13.4 3 11.5 3 8.5v-4z" /></S>;
export const IcExpand = () => <S><path d="M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5L9 7M2.5 13.5L7 9" /></S>;
export const IcClose = () => <S><path d="M4 4l8 8M12 4l-8 8" /></S>;
export const IcWarnTri = () => <S><path d="M8 2.8L14 13H2z" /><path d="M8 6.6v3M8 11.4v.1" /></S>;
export const IcRetryArc = () => <S><path d="M13 8a5 5 0 1 1-1.5-3.6" /><path d="M13 2.8v2.7h-2.7" /></S>;
export const IcChevron = () => <S><path d="M6 4l4 4-4 4" /></S>;
export const IcFolder = () => <S><path d="M2 4.5h4l1.5 1.5H14v6.5H2z" /></S>;
export const IcGit = () => <S><circle cx="5" cy="4.5" r="1.8" /><circle cx="5" cy="11.5" r="1.8" /><circle cx="11" cy="8" r="1.8" /><path d="M5 6.3v3.4M6.8 5.2L9.5 7M6.8 10.8L9.5 9" /></S>;
export const IcCommit = () => <S><circle cx="8" cy="8" r="2" /><path d="M8 2v4M8 10v4" /></S>;
export const IcMic = () => <S width={13}><rect x="6" y="2" width="4" height="8" rx="2" /><path d="M3.5 8a4.5 4.5 0 009 0M8 12.5V15" /></S>;
export const IcStar = () => <S width={13}><path d="M8 2.2l1.75 3.55 3.92.57-2.84 2.77.67 3.91L8 11.16 4.5 13l.67-3.91-2.84-2.77 3.92-.57z" /></S>;
// effort gauge (P4): speedometer dome + needle — the effort-selector affordance
export const IcGauge = () => <S><path d="M2.8 11.2a5.2 5.2 0 0 1 10.4 0" /><path d="M8 11.2l2.6-2.4" /></S>;
export const IcBrowserCard = () => <S><rect x="2.5" y="3" width="11" height="9" rx="1.5" /><path d="M2.5 6h11" /></S>;
// Unified background-process glyph: a play-in-circle, stroke-weight 1.3 to match
// the top icon strip. Used in the Tareas card + inline transcript rows + chip.
export const IcBgProc = () => <S><circle cx="8" cy="8" r="5.5" /><path d="M6.6 5.7l3.7 2.3-3.7 2.3z" /></S>;
export const IcRead = () => <S><rect x="2.5" y="3" width="11" height="10" rx="1.5" /><path d="M5 6h6M5 8.5h6M5 11h3.5" /></S>;
// Filled, not stroked: the same rounded square the composer's stop button
// draws, so one shape means "end this" wherever it appears.
export const IcStopSquare = () => <svg viewBox="0 0 16 16" fill="currentColor"><rect x="3.5" y="3.5" width="9" height="9" rx="2" /></svg>;
// (removed 2026-07-25) IcTool — a wrench that read as an illegible squiggle at
// 14px and stood for nothing in particular. Every former use now takes a mark
// that names its actual action; the generic tool call uses IcActPlug.
export const IcStampCopy = () => <S><rect x="5" y="5" width="8" height="8" rx="1.5" /><path d="M3 11V4a1 1 0 011-1h7" /></S>;

// ── Action glyphs for a single tool row (approved mock toolrow-names, 2026-07-25)
// One mark per ACTION, not per tool: the row says «Ejecutar un comando» with a
// terminal, «Editar un archivo» with a pencil. Read/search/web reuse the marks
// already in this file so the transcript keeps ONE icon vocabulary.
export const IcActRun = () => <S><rect x="1.8" y="2.8" width="12.4" height="10.4" rx="2" /><path d="M4.6 6.4l2.2 1.9-2.2 1.9M8.6 10.6h3" /></S>;
export const IcActEdit = () => <S><path d="M10.9 2.3l2.8 2.8-8 8H2.9v-2.8z" /><path d="M9.4 3.8l2.8 2.8" /></S>;
export const IcActWrite = () => <S><path d="M3.4 13.4V4a1.4 1.4 0 011.4-1.4h4l3.8 3.8v7a1.4 1.4 0 01-1.4 1.4H4.8a1.4 1.4 0 01-1.4-1.4z" /><path d="M8.6 2.8v3.4h3.8" /></S>;
export const IcActFetch = () => <S><path d="M8 2.6v7.2M8 9.8L5.4 7.2M8 9.8l2.6-2.6" /><path d="M2.8 11.4v1.2a1 1 0 001 1h8.4a1 1 0 001-1v-1.2" /></S>;
export const IcActPlan = () => <S><path d="M2.6 4.4l1.5 1.5 2.3-2.6M2.6 9.9l1.5 1.5 2.3-2.6" /><path d="M8.8 4.6h4.6M8.8 10.1h4.6" /></S>;
export const IcActDelete = () => <S><path d="M3.2 4.6h9.6M6.4 4.6V3.2h3.2v1.4M4.6 4.6l.6 8.2h5.6l.6-8.2" /></S>;
export const IcActMove = () => <S><path d="M2.6 8h9.4M9.4 5.2L12.6 8l-3.2 2.8" /><path d="M2.6 3.4v9.2" /></S>;
// MCP call: a plug — «something outside this agent did the work».
export const IcActPlug = () => <S><path d="M6 2.4v2.8M10 2.4v2.8" /><rect x="3.9" y="5.2" width="8.2" height="4.2" rx="1.5" /><path d="M8 9.4v4.2" /></S>;
export const IcActThink = () => <S><path d="M8 2.4l1.5 3.4 3.4 1.5-3.4 1.5L8 12.2 6.5 8.8 3.1 7.3l3.4-1.5z" /></S>;
export const IcActAgent = () => <S><circle cx="8" cy="5.2" r="2.4" /><path d="M3 13.2a5 5 0 0110 0" /></S>;
export const IcActChat = () => <S><path d="M2.6 4.4a1.6 1.6 0 011.6-1.6h7.6a1.6 1.6 0 011.6 1.6v4.8a1.6 1.6 0 01-1.6 1.6H6.4l-3 2.4v-2.4H4.2a1.6 1.6 0 01-1.6-1.6z" /></S>;

// One place maps an action key to its mark, so a new action can never render a
// row with no icon (see tool-names.ts ActionIcon). Every entry has to MEAN
// something at 14px — a generic catch-all glyph is what got the old wrench
// deleted (user, 2026-07-25).
const ACTION_ICON: Record<ActionIcon, () => ReactNode> = {
  run: IcActRun, read: IcRead, edit: IcActEdit, write: IcActWrite, search: IcSearch,
  web: IcBrowser, fetch: IcActFetch, plan: IcActPlan, delete: IcActDelete,
  move: IcActMove, think: IcActThink, agent: IcActAgent, chat: IcActChat,
  mcp: IcActPlug, tool: IcActPlug,
  // Lifecycle phases of running background work (approved mock rail-actions):
  // a process starting, its controls being applied, a blocked permission. Each
  // mark depicts its own phase — none of them is a stand-in.
  proc: IcBgProc, config: IcGear, shield: IcShield,
};
export function ActionGlyph({ icon }: { icon: ActionIcon }) {
  const Glyph = ACTION_ICON[icon] ?? IcActPlug;
  return <Glyph />;
}
export const IcStampUp = () => <S><path d="M8 13V6M8 6l3 3M8 6L5 9" /><path d="M3 3h10" /></S>;
export const IcStampDown = () => <S><path d="M8 3v7M8 10L5 7M8 10l3-3" /><path d="M3 13h10" /></S>;

// Model-family brand marks for subagent rows (paths verbatim from the approved
// subagent mock). Standalone viewBoxes, not the 16px S wrapper. The GPT knot
// inherits currentColor; the Claude burst keeps its brand hue so both read at
// ~15px. Which one shows is driven by the subagent's provider brand.
export const IcGpt = () => (
  <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
    <path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997z" />
  </svg>
);
export const IcClaude = () => (
  <svg viewBox="0 0 20 20" fill="none" aria-hidden="true">
    <g stroke="#cc7a52" strokeWidth="1.5" strokeLinecap="round">
      <line x1="10" y1="10" x2="10" y2="3.5" />
      <line x1="10" y1="10" x2="13.25" y2="4.37" />
      <line x1="10" y1="10" x2="15.63" y2="6.75" />
      <line x1="10" y1="10" x2="16.5" y2="10" />
      <line x1="10" y1="10" x2="15.63" y2="13.25" />
      <line x1="10" y1="10" x2="13.25" y2="15.63" />
      <line x1="10" y1="10" x2="10" y2="16.5" />
      <line x1="10" y1="10" x2="6.75" y2="15.63" />
      <line x1="10" y1="10" x2="4.37" y2="13.25" />
      <line x1="10" y1="10" x2="3.5" y2="10" />
      <line x1="10" y1="10" x2="4.37" y2="6.75" />
      <line x1="10" y1="10" x2="6.75" y2="4.37" />
    </g>
  </svg>
);
// Pick the brand mark for a subagent by its provider brand.
export function ModelIcon({ provider }: { provider: string | null | undefined }) {
  if (provider === 'gpt') return <IcGpt />;
  if (provider === 'claude') return <IcClaude />;
  return null;
}
// Classic telephone handset — the call-button receiver, for a subagent's tool
// "call" count (llamadas). Filled so it reads at ~13px; own viewBox, not the 16px
// S wrapper.
export const IcPhone = () => (
  <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
    <path d="M6.62 10.79c1.44 2.83 3.76 5.14 6.59 6.59l2.2-2.2c.27-.27.67-.36 1.02-.24 1.12.37 2.33.57 3.57.57.55 0 1 .45 1 1V20c0 .55-.45 1-1 1-9.39 0-17-7.61-17-17 0-.55.45-1 1-1h3.5c.55 0 1 .45 1 1 0 1.25.2 2.45.57 3.57.11.35.03.74-.25 1.02l-2.2 2.2z" />
  </svg>
);
