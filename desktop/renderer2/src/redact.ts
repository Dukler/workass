// TS mirror of the daemon's redactSensitiveText (internal/acp/types.go). The
// daemon redacts every persisted/served copy of a transcript row at ingestion
// (PrepareTurn, BeginLiveSteer, session:save), so the renderer must paint the
// SAME bytes into its optimistic rows — otherwise the first reload visibly
// rewrites sentences when the daemon copy replaces the local one. Outputs must
// stay byte-identical to the Go function; the parity fixtures in
// tests/redact.test.ts are generated from the Go implementation.
// RE2 \s is [\t\n\f\r ]; spelled out so both engines match identical spans.
const SECRET_TEXT_RE = /(bearer[\t\n\f\r ]+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret(?:[_-]?(?:access[_-]?)?key)?|private[_-]?key|passw(?:or)?d|credential)["']?[\t\n\f\r ]*[:=][\t\n\f\r ]*)("[^"]*"|'[^']*'|[^\t\n\f\r ,;}]+)|(authorization[\t\n\f\r ]*:[\t\n\f\r ]*basic[\t\n\f\r ]+)[A-Za-z0-9+/=]+|:\/\/[^\t\n\f\r /:@]+:[^\t\n\f\r /@]+@|-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|\b(?:sk-ant-[A-Za-z0-9_-]{16,}|sk-(?:proj-)?[A-Za-z0-9_-]{32,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|xox[abposr]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})/gi;

export function redactSensitiveText(text: string): string {
  return text.replace(SECRET_TEXT_RE, (match) => {
    const lower = match.toLowerCase();
    if (lower.startsWith('bearer ')) return match.slice(0, 7) + '[redacted]';
    if (lower.startsWith('authorization')) {
      const i = lower.indexOf('basic');
      if (i >= 0) {
        const rest = match.slice(i + 5);
        return match.slice(0, i + 5) + rest.slice(0, rest.length - rest.replace(/^[\t\n\f\r ]+/, '').length) + '[redacted]';
      }
    }
    if (match.startsWith('://')) return match.slice(0, match.lastIndexOf(':') + 1) + '[redacted]@';
    if (match.startsWith('-----')) return '[redacted]';
    const colon = match.indexOf(':');
    const eq = match.indexOf('=');
    const sep = colon < 0 ? eq : eq < 0 ? colon : Math.min(colon, eq);
    return sep >= 0 ? match.slice(0, sep + 1) + '[redacted]' : '[redacted]';
  });
}
