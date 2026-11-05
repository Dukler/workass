// The Salidas tab of the activity pane: what this chat produced that a person
// opens. Read from what the transcript already holds, never from a second
// source:
//
//   · PÁGINA  — a page the agent published with workass_render_html (the
//               reference in that call's output, html-render.ts).
//   · IMAGEN  — an image in an assistant reply, an image a tool returned, or
//               a hosted artifact linked as an image.
//   · ARCHIVO — a hosted artifact (workass_host_artifact) linked in a reply.
//
// Left out on purpose: an ```html fence and a visualization have no durable
// reference to reopen (their frames are built in place), and a subagent's
// results belong to that subagent. Pure, so node --test can read it directly.

import type { MessageImage, Msg, ToolEvent } from './store/types';
import { htmlRenderFromToolCall } from './markdown/html-render.ts';
import { isArtifactLink, parseArtifactLink } from './connected-artifacts.ts';

export type ChatOutputKind = 'page' | 'image' | 'file';

export interface ChatOutput {
  key: string;
  kind: ChatOutputKind;
  title: string;
  messageId: string;
  /** A hosted artifact path (a page, a file, or an image hosted as one). */
  target?: string;
  /** An image whose bytes ride the transcript. */
  image?: MessageImage;
  /** The hosted artifact a link names, so a link to a listed page is that page. */
  artifactId?: string;
}

export const OUTPUT_TAG: Record<ChatOutputKind, string> = { page: 'PÁGINA', image: 'IMAGEN', file: 'ARCHIVO' };

// A Markdown link or image: `[label](target "title")`, `![label](<target>)`.
const MD_LINK = /(!?)\[([^\]\n]{0,300})\]\(\s*<?([^()\s<>]+)>?(?:\s+"[^"\n]*")?\s*\)/g;
const FENCE = /(^|\n)\s*(```|~~~)[\s\S]*?(\n\s*\2|$)/g;

// workass_host_artifact's ready-made Markdown reads «Open <name>»; the row
// already says Abrir, so the verb is dropped from the title.
function plainLabel(label: string): string {
  return label.replace(/[*_`~]/g, '').replace(/\s+/g, ' ').trim().replace(/^(open|abrir)\s+(?=\S)/i, '');
}

function artifactName(target: string): string {
  const parsed = parseArtifactLink(target);
  const tail = parsed?.path.split('/').filter(Boolean).pop() ?? '';
  try { return decodeURIComponent(tail) || parsed?.artifactId || target; } catch { return tail || target; }
}

function artifactKey(target: string): string {
  const parsed = parseArtifactLink(target);
  return parsed ? `${parsed.machineId ?? ''}/${parsed.artifactId}${parsed.path}` : target;
}

function imageKey(image: MessageImage, fallback: string): string {
  return image.deferredImageRef || image.source || fallback;
}

function messageOutputs(msg: Msg): ChatOutput[] {
  const out: ChatOutput[] = [];
  const pages = new Set<string>();
  for (const event of msg.events) {
    if (event.kind !== 'tool') continue;
    const tool = event as ToolEvent;
    // Only the chat's own calls: a subagent reports to its parent.
    if (tool.subagentId) continue;
    const reference = htmlRenderFromToolCall(tool);
    if (reference && !pages.has(reference.id)) {
      pages.add(reference.id);
      out.push({ key: `page:${reference.id}`, kind: 'page', title: reference.title, messageId: msg.id, target: reference.urlPath, artifactId: reference.id });
    }
    (tool.images ?? []).forEach((image, index) => {
      out.push({
        key: `img:${imageKey(image, `${msg.id}:${tool.key}:${index}`)}`, kind: 'image',
        title: image.name || tool.title || 'Imagen', messageId: msg.id, image,
      });
    });
  }
  (msg.images ?? []).forEach((image, index) => {
    out.push({ key: `img:${imageKey(image, `${msg.id}:${index}`)}`, kind: 'image', title: image.name || 'Imagen', messageId: msg.id, image });
  });
  const text = [msg.content, msg.result].filter((part): part is string => typeof part === 'string' && part.includes('/workass/'))
    .join('\n').replace(FENCE, '\n');
  for (const match of text.matchAll(MD_LINK)) {
    const target = match[3];
    if (!isArtifactLink(target)) continue;
    const parsed = parseArtifactLink(target);
    if (!parsed) continue;
    const kind: ChatOutputKind = match[1] ? 'image' : 'file';
    out.push({
      key: `${kind}:${artifactKey(target)}`, kind,
      title: plainLabel(match[2]) || artifactName(target), messageId: msg.id, target, artifactId: parsed.artifactId,
    });
  }
  return out;
}

// A settled reply never changes; a live one is re-read when anything it holds
// moved. The signature is cheap next to the scan it saves.
const cache = new WeakMap<Msg, { sig: string; items: ChatOutput[] }>();

function signature(msg: Msg): string {
  let tools = 0;
  let settled = 0;
  let images = 0;
  for (const event of msg.events) {
    if (event.kind !== 'tool') continue;
    tools += 1;
    if (event.status === 'completed') settled += 1;
    images += event.images?.length ?? 0;
  }
  return `${msg.status}|${tools}|${settled}|${images}|${msg.images?.length ?? 0}|${msg.content.length}|${msg.result?.length ?? 0}`;
}

/** Newest first, one row per output however often it was linked. */
export function chatOutputs(messages: readonly Msg[] | null | undefined): ChatOutput[] {
  const seen = new Set<string>();
  const all: ChatOutput[] = [];
  for (const msg of messages ?? []) {
    if (msg.role !== 'assistant') continue;
    const sig = signature(msg);
    let hit = cache.get(msg);
    if (!hit || hit.sig !== sig) {
      hit = { sig, items: messageOutputs(msg) };
      cache.set(msg, hit);
    }
    for (const item of hit.items) {
      if (seen.has(item.key)) continue;
      seen.add(item.key);
      all.push(item);
    }
  }
  // A link to a page the chat published is that page, wherever it was linked.
  const pages = new Set(all.filter((item) => item.kind === 'page').map((item) => item.artifactId));
  return all.filter((item) => item.kind === 'page' || !item.artifactId || !pages.has(item.artifactId)).reverse();
}
