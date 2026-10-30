// The outcome word on a Workass notice line.
//
// It was a boolean before: `status === 'failed' ? 'falló' : 'terminó'`. That
// read correctly while every notice was a subagent on THIS machine settling,
// where the only two answers were "finished" and "failed". The fleet runtime
// adds two statuses that are neither (docs/specs/fleet-runtime.md §7):
//
//   home_down — the machine hosting this work has not answered. It means
//     UNKNOWN, not dead, and the spec forbids a watcher converting it into an
//     outcome. Rendering it as "terminó" would be exactly that conversion, and
//     a transcript that says a dark machine's agent finished is a lie.
//   attention — the work is still running and is waiting on a decision.
//
// No new row, no new chrome: the same quiet line already shows a status word,
// and this is the correct value of that existing field. Every word stays gray,
// like every outcome word — never red.
export function noticeWord(status: string | undefined): string {
  switch (status) {
    case 'failed': return 'falló';
    case 'home_down': return 'sin contacto';
    case 'attention': return 'espera una decisión';
    default: return 'terminó';
  }
}
