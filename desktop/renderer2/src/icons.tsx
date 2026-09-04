// SVG icons — paths lifted verbatim from the approved mock (workass-one.html).
import { useId, type ReactNode } from 'react';
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
// Provider marks below preserve the geometry published by each provider. They
// stay inline so packaged Workass never depends on a network request to paint
// Settings or the model picker.
export const IcDevin = () => (
  <svg viewBox="0 0 425 425" fill="currentColor" aria-hidden="true">
    <path d="M70 159.333V91.3471C70 88.3592 71.594 85.5983 74.1816 84.1044L133.043 50.1205C135.631 48.6265 138.819 48.6265 141.407 50.1205L200.269 84.1044C202.856 85.5983 204.45 88.3592 204.45 91.3471V126.068C204.708 137.606 210.806 148.734 221.531 154.926C232.256 161.117 244.942 160.834 255.063 155.289L285.132 137.929C287.719 136.435 290.907 136.435 293.495 137.929L352.357 171.913C354.944 173.406 356.538 176.167 356.538 179.155V247.123C356.538 250.111 354.944 252.872 352.357 254.366L293.495 288.35C290.907 289.844 287.719 289.844 285.132 288.35L255.306 271.13C245.146 265.456 232.344 265.117 221.534 271.358C210.809 277.55 204.711 288.678 204.453 300.215V334.926C204.453 337.914 202.859 340.675 200.271 342.169L141.41 376.153C138.822 377.647 135.634 377.647 133.046 376.153L74.1845 342.169C71.5969 340.675 70.0028 337.914 70.0028 334.926V266.959C70.0029 263.971 71.5969 261.21 74.1845 259.716L133.046 225.732C135.634 224.238 138.822 224.238 141.41 225.732L171.547 243.132C181.656 248.638 194.306 248.906 205.005 242.729C215.815 236.488 221.922 225.231 222.088 213.595C221.83 202.057 215.732 189.737 205.008 183.545C194.283 177.353 181.597 177.636 171.476 183.181L141.269 200.72C138.67 202.229 135.461 202.228 132.864 200.716L74.1576 166.562C71.5835 165.065 70 162.311 70 159.333Z" />
  </svg>
);
export const IcOmp = () => (
  <svg viewBox="0 0 120 90" aria-hidden="true">
    <rect x="10" y="8" width="100" height="12" rx="2" fill="currentColor" />
    <rect x="25" y="20" width="12" height="62" rx="2" fill="currentColor" />
    <rect x="75" y="20" width="12" height="45" rx="2" fill="currentColor" />
    <rect x="71" y="55" width="20" height="16" rx="3" fill="#f97316" />
    <rect x="76" y="59" width="3" height="8" rx="1" fill="var(--bg)" />
    <rect x="82" y="59" width="3" height="8" rx="1" fill="var(--bg)" />
    <circle cx="18" cy="14" r="2" fill="#f97316" opacity=".8" />
    <circle cx="102" cy="14" r="2" fill="#f97316" opacity=".8" />
  </svg>
);
export const IcOpenCode = () => (
  <svg viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
    <path fillRule="evenodd" clipRule="evenodd" d="M13 14H3V2H13V14ZM10.5 4.4H5.5V11.6H10.5V4.4Z" />
  </svg>
);
export const IcQwen = () => (
  <svg viewBox="0 0 24 24" fill="#6950ef" aria-hidden="true">
    <path d="M23.919 14.545 20.817 9.17l1.47-2.544a.56.56 0 0 0 0-.566l-1.633-2.83a.57.57 0 0 0-.49-.283h-6.207L12.487.402a.57.57 0 0 0-.49-.284H8.732a.56.56 0 0 0-.49.284L5.139 5.775h-2.94a.56.56 0 0 0-.49.284L.077 8.887a.56.56 0 0 0 0 .567L3.18 14.83l-1.47 2.545a.56.56 0 0 0 0 .566l1.634 2.83a.57.57 0 0 0 .49.283h6.205l1.47 2.545a.57.57 0 0 0 .49.284h3.266a.57.57 0 0 0 .49-.284l3.104-5.375h2.94a.57.57 0 0 0 .49-.283l1.634-2.828a.55.55 0 0 0-.004-.568M8.733.686l1.634 2.828-1.634 2.828H21.8L20.164 9.17H7.425L5.63 6.06Zm1.306 19.801-6.205-.002 1.634-2.83h3.265L2.201 6.344h3.267q3.182 5.517 6.367 11.032zm10.124-5.66L18.53 12l-6.532 11.315-1.634-2.83c2.129-3.673 4.25-7.351 6.373-11.028h3.592l3.102 5.374z" />
  </svg>
);
export const IcLMStudio = () => {
  const gradient = useId();
  return (
    <svg viewBox="0 0 512 512" fill="none" aria-hidden="true">
      <path d="M0 179.2C0 116.474 0 85.1112 12.2073 61.1531C22.9451 40.0789 40.0789 22.9451 61.1531 12.2073C85.1112 0 116.474 0 179.2 0H332.8C395.526 0 426.889 0 450.847 12.2073C471.921 22.9451 489.055 40.0789 499.793 61.1531C512 85.1112 512 116.474 512 179.2V332.8C512 395.526 512 426.889 499.793 450.847C489.055 471.921 471.921 489.055 450.847 499.793C426.889 512 395.526 512 332.8 512H179.2C116.474 512 85.1112 512 61.1531 499.793C40.0789 489.055 22.9451 471.921 12.2073 450.847C0 426.889 0 395.526 0 332.8V179.2Z" fill={`url(#${gradient})`} />
      <rect opacity=".25" x="128" y="84" width="224" height="44" rx="22" fill="white" />
      <rect x="64" y="84" width="224" height="44" rx="22" fill="white" />
      <rect opacity=".25" x="224" y="144" width="224" height="44" rx="22" fill="white" />
      <rect x="160" y="144" width="224" height="44" rx="22" fill="white" />
      <rect opacity=".25" x="168" y="204" width="224" height="44" rx="22" fill="white" />
      <rect x="104" y="204" width="224" height="44" rx="22" fill="white" />
      <rect opacity=".25" x="112" y="264" width="224" height="44" rx="22" fill="white" />
      <rect x="48" y="264" width="224" height="44" rx="22" fill="white" />
      <rect opacity=".25" x="176" y="324" width="224" height="44" rx="22" fill="white" />
      <rect x="112" y="324" width="224" height="44" rx="22" fill="white" />
      <rect opacity=".25" x="304" y="384" width="152" height="44" rx="22" fill="white" />
      <rect x="240" y="384" width="152" height="44" rx="22" fill="white" />
      <defs><linearGradient id={gradient} x1="-219.792" y1="229.426" x2="239.06" y2="702.601" gradientUnits="userSpaceOnUse"><stop stopColor="#6e7ef3" /><stop offset="1" stopColor="#4f13be" /></linearGradient></defs>
    </svg>
  );
};
export const IcOllama = () => (
  <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
    <path d="M16.361 10.26a.894.894 0 0 0-.558.47l-.072.148.001.207c0 .193.004.217.059.353.076.193.152.312.291.448.24.238.51.3.872.205a.86.86 0 0 0 .517-.436.752.752 0 0 0 .08-.498c-.064-.453-.33-.782-.724-.897a1.06 1.06 0 0 0-.466 0zm-9.203.005c-.305.096-.533.32-.65.639a1.187 1.187 0 0 0-.06.52c.057.309.31.59.598.667.362.095.632.033.872-.205.14-.136.215-.255.291-.448.055-.136.059-.16.059-.353l.001-.207-.072-.148a.894.894 0 0 0-.565-.472 1.02 1.02 0 0 0-.474.007Zm4.184 2c-.131.071-.223.25-.195.383.031.143.157.288.353.407.105.063.112.072.117.136.004.038-.01.146-.029.243-.02.094-.036.194-.036.222.002.074.07.195.143.253.064.052.076.054.255.059.164.005.198.001.264-.03.169-.082.212-.234.15-.525-.052-.243-.042-.28.087-.355.137-.08.281-.219.324-.314a.365.365 0 0 0-.175-.48.394.394 0 0 0-.181-.033c-.126 0-.207.03-.355.124l-.085.053-.053-.032c-.219-.13-.259-.145-.391-.143a.396.396 0 0 0-.193.032zm.39-2.195c-.373.036-.475.05-.654.086-.291.06-.68.195-.951.328-.94.46-1.589 1.226-1.787 2.114-.04.176-.045.234-.045.53 0 .294.005.357.043.524.264 1.16 1.332 2.017 2.714 2.173.3.033 1.596.033 1.896 0 1.11-.125 2.064-.727 2.493-1.571.114-.226.169-.372.22-.602.039-.167.044-.23.044-.523 0-.297-.005-.355-.045-.531-.288-1.29-1.539-2.304-3.072-2.497a6.873 6.873 0 0 0-.855-.031zm.645.937a3.283 3.283 0 0 1 1.44.514c.223.148.537.458.671.662.166.251.26.508.303.82.02.143.01.251-.043.482-.08.345-.332.705-.672.957a3.115 3.115 0 0 1-.689.348c-.382.122-.632.144-1.525.138-.582-.006-.686-.01-.853-.042-.57-.107-1.022-.334-1.35-.68-.264-.28-.385-.535-.45-.946-.03-.192.025-.509.137-.776.136-.326.488-.73.836-.963.403-.269.934-.46 1.422-.512.187-.02.586-.02.773-.002zm-5.503-11a1.653 1.653 0 0 0-.683.298C5.617.74 5.173 1.666 4.985 2.819c-.07.436-.119 1.04-.119 1.503 0 .544.064 1.24.155 1.721.02.107.031.202.023.208a8.12 8.12 0 0 1-.187.152 5.324 5.324 0 0 0-.949 1.02 5.49 5.49 0 0 0-.94 2.339 6.625 6.625 0 0 0-.023 1.357c.091.78.325 1.438.727 2.04l.13.195-.037.064c-.269.452-.498 1.105-.605 1.732-.084.496-.095.629-.095 1.294 0 .67.009.803.088 1.266.095.555.288 1.143.503 1.534.071.128.243.393.264.407.007.003-.014.067-.046.141a7.405 7.405 0 0 0-.548 1.873c-.062.417-.071.552-.071.991 0 .56.031.832.148 1.279L3.42 24h1.478l-.05-.091c-.297-.552-.325-1.575-.068-2.597.117-.472.25-.819.498-1.296l.148-.29v-.177c0-.165-.003-.184-.057-.293a.915.915 0 0 0-.194-.25 1.74 1.74 0 0 1-.385-.543c-.424-.92-.506-2.286-.208-3.451.124-.486.329-.918.544-1.154a.787.787 0 0 0 .223-.531c0-.195-.07-.355-.224-.522a3.136 3.136 0 0 1-.817-1.729c-.14-.96.114-2.005.69-2.834.563-.814 1.353-1.336 2.237-1.475.199-.033.57-.028.776.01.226.04.367.028.512-.041.179-.085.268-.19.374-.431.093-.215.165-.333.36-.576.234-.29.46-.489.822-.729.413-.27.884-.467 1.352-.561.17-.035.25-.04.569-.04.319 0 .398.005.569.04a4.07 4.07 0 0 1 1.914.997c.117.109.398.457.488.602.034.057.095.177.132.267.105.241.195.346.374.43.14.068.286.082.503.045.343-.058.607-.053.943.016 1.144.23 2.14 1.173 2.581 2.437.385 1.108.276 2.267-.296 3.153-.097.15-.193.27-.333.419-.301.322-.301.722-.001 1.053.493.539.801 1.866.708 3.036-.062.772-.26 1.463-.533 1.854a2.096 2.096 0 0 1-.224.258.916.916 0 0 0-.194.25c-.054.109-.057.128-.057.293v.178l.148.29c.248.476.38.823.498 1.295.253 1.008.231 2.01-.059 2.581a.845.845 0 0 0-.044.098c0 .006.329.009.732.009h.73l.02-.074.036-.134c.019-.076.057-.3.088-.516.029-.217.029-1.016 0-1.258-.11-.875-.295-1.57-.597-2.226-.032-.074-.053-.138-.046-.141.008-.005.057-.074.108-.152.376-.569.607-1.284.724-2.228.031-.26.031-1.378 0-1.628-.083-.645-.182-1.082-.348-1.525a6.083 6.083 0 0 0-.329-.7l-.038-.064.131-.194c.402-.604.636-1.262.727-2.04a6.625 6.625 0 0 0-.024-1.358 5.512 5.512 0 0 0-.939-2.339 5.325 5.325 0 0 0-.95-1.02 8.097 8.097 0 0 1-.186-.152.692.692 0 0 1 .023-.208c.208-1.087.201-2.443-.017-3.503-.19-.924-.535-1.658-.98-2.082-.354-.338-.716-.482-1.15-.455-.996.059-1.8 1.205-2.116 3.01a6.805 6.805 0 0 0-.097.726c0 .036-.007.066-.015.066a.96.96 0 0 1-.149-.078A4.857 4.857 0 0 0 12 3.03c-.832 0-1.687.243-2.456.698a.958.958 0 0 1-.148.078c-.008 0-.015-.03-.015-.066a6.71 6.71 0 0 0-.097-.725C8.997 1.392 8.337.319 7.46.048a2.096 2.096 0 0 0-.585-.041Zm.293 1.402c.248.197.523.759.682 1.388.03.113.06.244.069.292.007.047.026.152.041.233.067.365.098.76.102 1.24l.002.475-.12.175-.118.178h-.278c-.324 0-.646.041-.954.124l-.238.06c-.033.007-.038-.003-.057-.144a8.438 8.438 0 0 1 .016-2.323c.124-.788.413-1.501.696-1.711.067-.05.079-.049.157.013zm9.825-.012c.17.126.358.46.498.888.28.854.36 2.028.212 3.145-.019.14-.024.151-.057.144l-.238-.06a3.693 3.693 0 0 0-.954-.124h-.278l-.119-.178-.119-.175.002-.474c.004-.669.066-1.19.214-1.772.157-.623.434-1.185.68-1.382.078-.062.09-.063.159-.012z" />
  </svg>
);
export const IcOMLX = () => {
  const gradient = useId();
  return (
    <svg viewBox="0 0 160 160" aria-hidden="true">
      <defs><linearGradient id={gradient} x1="0" y1="0" x2="0" y2="1"><stop offset="0" stopColor="#2d2d2d" /><stop offset="1" stopColor="#1a1a1a" /></linearGradient></defs>
      <rect x="10" y="10" width="140" height="140" rx="32" fill={`url(#${gradient})`} />
      <g transform="translate(25 25) scale(.0221)"><g transform="translate(0 4970) scale(1 -1)" fill="#fff">
        <path d="M2275 4349c-408-39-769-207-1056-492-196-194-333-428-418-715-47-158-67-281-101-617-66-662-116-944-245-1387-102-352-271-774-420-1051-19-35-35-70-35-76 0-8 50-11 163-11h164l80 168c168 348 303 739 408 1175 73 307 109 532 155 982 52 500 72 627 122 785 162 507 570 860 1096 951 155 26 389 26 544 0 221-38 440-129 620-258 45-32 152-126 237-209 86-82 178-165 204-183 106-72 312-150 495-186l32-7-86-54c-110-69-170-117-267-212-93-91-143-154-191-243-105-191-130-406-75-623 29-115 81-239 217-516 234-480 343-769 411-1091 36-172 48-252 57-381l7-98h316l-4 28c-2 15-6 66-9 113-14 218-95 560-201 849-81 220-165 407-363 810-120 245-147 320-161 443-38 338 202 621 766 906 131 65 166 126 106 183-33 31-86 47-288 88-177 36-274 61-370 97-140 52-190 88-377 270-140 137-202 189-300 254-378 250-782 351-1233 308zm775-958c-57-11-122-53-154-99-41-57-49-158-18-218 29-56 66-92 120-117 153-69 323 37 325 203 1 147-131 259-273 231zM1985 1391c-68-31-70-40-66-271 1-114-2-243-9-290-40-307-124-555-255-754-25-38-45-71-45-72 0-2 79-4 176-4h175l54 113c117 247 182 512 201 812 7 126-9 319-32 374-26 63-86 111-136 111-13 0-41-9-63-19z" />
      </g></g>
    </svg>
  );
};
// Pick the brand mark for a subagent by its provider brand.
export function ModelIcon({ provider }: { provider: string | null | undefined }) {
  if (provider === 'gpt') return <IcGpt />;
  if (provider === 'claude') return <IcClaude />;
  if (provider === 'devin') return <IcDevin />;
  if (provider === 'omp') return <IcOmp />;
  if (provider === 'opencode') return <IcOpenCode />;
  if (provider === 'qwen') return <IcQwen />;
  if (provider === 'lmstudio') return <IcLMStudio />;
  if (provider === 'ollama') return <IcOllama />;
  if (provider === 'omlx') return <IcOMLX />;
  return null;
}
export function hasModelIcon(provider: string | null | undefined): boolean {
  return provider === 'gpt' || provider === 'claude' || provider === 'devin' || provider === 'omp' || provider === 'opencode' || provider === 'qwen' || provider === 'lmstudio' || provider === 'ollama' || provider === 'omlx';
}

// Old remote daemons may not yet send AssistantBrand. Keep that compatibility
// mapping in this one presentation-only provider boundary; Settings itself
// remains generic and falls back to a letter only when no verified mark exists.
export function providerIconBrand(providerId: string | null | undefined, assistantBrand: string | null | undefined): string {
  const explicit = String(assistantBrand ?? '').trim().toLowerCase();
  if (hasModelIcon(explicit)) return explicit;
  switch (String(providerId ?? '').trim().toLowerCase()) {
    case 'codex': return 'gpt';
    case 'claude': return 'claude';
    case 'devin': return 'devin';
    case 'omp': return 'omp';
    case 'opencode': return 'opencode';
    case 'qwen': return 'qwen';
    case 'local-lmstudio': return 'lmstudio';
    case 'local-ollama': return 'ollama';
    case 'local-omlx': return 'omlx';
    default: return '';
  }
}
// Classic telephone handset — the call-button receiver, for a subagent's tool
// "call" count (llamadas). Filled so it reads at ~13px; own viewBox, not the 16px
// S wrapper.
export const IcPhone = () => (
  <svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
    <path d="M6.62 10.79c1.44 2.83 3.76 5.14 6.59 6.59l2.2-2.2c.27-.27.67-.36 1.02-.24 1.12.37 2.33.57 3.57.57.55 0 1 .45 1 1V20c0 .55-.45 1-1 1-9.39 0-17-7.61-17-17 0-.55.45-1 1-1h3.5c.55 0 1 .45 1 1 0 1.25.2 2.45.57 3.57.11.35.03.74-.25 1.02l-2.2 2.2z" />
  </svg>
);
