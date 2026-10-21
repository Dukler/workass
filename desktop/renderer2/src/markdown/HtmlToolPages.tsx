// Pages the agent published with workass_render_html. The call itself stays an
// ordinary shell row in the Worked fold (which is also all an older renderer
// shows); its page is a result, so it renders where the call landed, in the
// same frame as an ```html fence (t3code renders html_render the same way, as
// its own timeline row: apps/web/src/session-logic.ts:747-760).
import { useEffect, useState } from 'react';
import type { ToolEvent } from '../store/types';
import { connectedArtifactURL } from '../connected-artifacts';
import { store } from '../store/store';
import { htmlRenderFromToolCall, storedHtmlSource, HTML_RENDER_MAX_SOURCE, type HtmlRenderReference } from './html-render';
import { HtmlBlock } from './HtmlBlock';

// The page bytes come from the daemon's artifact host (local, or a mounted
// machine through the shell's artifact bridge). Fetched once per URL.
const sources = new Map<string, Promise<string>>();
const SOURCE_LIMIT = 32;

function loadPageSource(url: string): Promise<string> {
  const cached = sources.get(url);
  if (cached) return cached;
  const request = fetch(url, { cache: 'no-store', credentials: 'same-origin', referrerPolicy: 'no-referrer' })
    .then(async (response) => {
      if (!response.ok) throw new Error(`page unavailable (${response.status})`);
      const source = storedHtmlSource(await response.text());
      if (!source || source.length > HTML_RENDER_MAX_SOURCE) throw new Error('page is empty or too large');
      return source;
    });
  sources.set(url, request);
  while (sources.size > SOURCE_LIMIT) sources.delete(sources.keys().next().value as string);
  request.catch(() => { if (sources.get(url) === request) sources.delete(url); });
  return request;
}

function ToolHtmlPage({ reference, artifactOrigin }: { reference: HtmlRenderReference; artifactOrigin?: string }) {
  const url = connectedArtifactURL(artifactOrigin ?? '', reference.urlPath, undefined, undefined, store.localMachineId());
  const [source, setSource] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    setSource(null);
    if (!url) return () => { live = false; };
    loadPageSource(url).then((text) => { if (live) setSource(text); }, () => { /* the call's own row remains */ });
    return () => { live = false; };
  }, [url]);
  if (source === null) return null;
  return <HtmlBlock raw={source} closed streaming={false} title={reference.title} />;
}

export function ToolHtmlPages({ tools, artifactOrigin }: { tools: ToolEvent[]; artifactOrigin?: string }) {
  const pages = tools.flatMap((tool) => {
    // Only the chat's own calls: a subagent reports to its parent, and its
    // call stays an ordinary row inside the subagent's work.
    if (tool.subagentId) return [];
    const reference = htmlRenderFromToolCall(tool);
    return reference ? [{ tool, reference }] : [];
  });
  if (!pages.length) return null;
  return <>{pages.map(({ tool, reference }) => <ToolHtmlPage key={tool.key} reference={reference} artifactOrigin={artifactOrigin} />)}</>;
}
