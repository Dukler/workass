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
// P4 (supervised process children) adds three more that are none of the above:
//
//   timed_out — the job was stopped at the max_runtime it declared. Whether
//     its work was complete is unknown, so «terminó» would be a claim and
//     «falló» would be a different one.
//   lost — its supervisor is gone and wrote no outcome. The job may even still
//     be running; nobody is left who could say. This is the one word that must
//     never read like an ending.
//   stalled — the job went quiet past the stale_after it declared and is STILL
//     RUNNING. A notice that read like an ending would have somebody clean up
//     after work that is still going.
//
// No new row, no new chrome: the same quiet line already shows a status word,
// and this is the correct value of that existing field. Every word stays gray,
// like every outcome word — never red.
export function noticeWord(status: string | undefined): string {
  switch (status) {
    case 'failed': return 'falló';
    case 'home_down': return 'sin contacto';
    case 'attention': return 'espera una decisión';
    case 'timed_out': return 'agotó su tiempo';
    case 'lost': return 'se perdió sin rastro';
    case 'stalled': return 'sigue sin avanzar';
    default: return 'terminó';
  }
}
