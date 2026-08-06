export interface VisualizeSpec {
  path: string;
  mode?: 'wide';
  title?: string;
}

export interface VisualizeParseResult {
  spec?: VisualizeSpec;
  error?: string;
}

const START = 'visualize';
const END = '';

// The agent-side skill puts this reference on its own line. Keeping the parser
// line-oriented prevents malformed JSON from swallowing neighboring markdown.
export function parseVisualizeReference(line: string): VisualizeParseResult | null {
  const raw = line.trim();
  if (!raw.startsWith(START) || !raw.endsWith(END)) return null;
  const encoded = raw.slice(START.length, -END.length).trim();
  if (!encoded) return { error: 'missing visualization metadata' };

  let value: unknown;
  try {
    value = JSON.parse(encoded);
  } catch {
    return { error: 'invalid visualization metadata' };
  }
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return { error: 'visualization metadata must be an object' };
  }
  const record = value as Record<string, unknown>;
  const path = typeof record.path === 'string' ? record.path.trim() : '';
  if (!path || path.length > 4096 || path.includes('\u0000') || path.includes('://')) {
    return { error: 'visualization path is invalid' };
  }
  const mode = record.mode === undefined || record.mode === '' ? undefined : record.mode;
  if (mode !== undefined && mode !== 'wide') {
    return { error: 'visualization mode must be wide when provided' };
  }
  let title: string | undefined;
  if (record.title !== undefined) {
    if (typeof record.title !== 'string') return { error: 'visualization title is invalid' };
    title = record.title.trim();
    if (title.length > 200 || /[\u0000\r\n]/.test(title)) return { error: 'visualization title is invalid' };
  }
  return { spec: { path, ...(mode ? { mode } : {}), ...(title ? { title } : {}) } };
}
