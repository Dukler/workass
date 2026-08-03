// TS mirror of the daemon's redactSensitiveText (internal/acp/types.go). The
// daemon redacts every persisted/served copy of a transcript row at ingestion
// (PrepareTurn, BeginLiveSteer, session:save), so the renderer must paint the
// SAME bytes into its optimistic rows — otherwise the first reload visibly
// rewrites sentences when the daemon copy replaces the local one. Outputs must
// stay byte-identical to the Go function; the parity fixtures in
// tests/redact.test.ts are generated from the Go implementation.
// RE2 \s is [\t\n\f\r ]; spelled out so both engines match identical spans.
const SECRET_TEXT_RE = /(bearer[\t\n\f\r ]+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password|credential)[\t\n\f\r ]*[:=][\t\n\f\r ]*)("[^"]*"|'[^']*'|[^\t\n\f\r ,;}]+)/gi;

export function redactSensitiveText(text: string): string {
  return text.replace(SECRET_TEXT_RE, (match) => {
    if (match.toLowerCase().startsWith('bearer ')) return match.slice(0, 7) + '[redacted]';
    const colon = match.indexOf(':');
    const eq = match.indexOf('=');
    const sep = colon < 0 ? eq : eq < 0 ? colon : Math.min(colon, eq);
    return sep >= 0 ? match.slice(0, sep + 1) + '[redacted]' : '[redacted]';
  });
}
