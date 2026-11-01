// node:test reporter that writes one JSON line per test outcome, so the suite
// runner knows which test failed, in which file and why without parsing the
// human spec output. It is added beside the spec reporter, never instead of it.

function firstLine(value) {
  return String(value ?? '').split(/\r?\n/).map(line => line.trim()).find(Boolean)?.slice(0, 300) ?? '';
}

export function testEventLine(event) {
  if (event.type !== 'test:pass' && event.type !== 'test:fail') return null;
  const { name, nesting, file, skip, todo, details } = event.data;
  const error = details?.error;
  return `${JSON.stringify({
    outcome: event.type === 'test:pass' ? 'pass' : 'fail',
    name, nesting, file,
    skip: skip !== undefined,
    todo: todo !== undefined,
    ...(event.type === 'test:fail' ? { failureType: error?.failureType, cause: firstLine(error?.cause?.message ?? error?.cause ?? error?.message) } : {}),
  })}\n`;
}

export default async function* testEventsReporter(source) {
  for await (const event of source) {
    const line = testEventLine(event);
    if (line) yield line;
  }
}
