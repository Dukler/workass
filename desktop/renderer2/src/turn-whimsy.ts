// Claude Code's spinner speaks in whimsical gerunds ("Fermenting…",
// "Percolating…"); the turn pulse borrows the idiom in rioplatense. One word
// per turn, chosen deterministically from the job id so re-renders and
// reloads never make the label jitter mid-turn — the next turn draws a new
// one. Honest states (tool names, retries, compaction) never use these.
const WHIMSY = [
  'Fermentando', 'Macerando', 'Rumiando', 'Cavilando', 'Percolando',
  'Amasando', 'Destilando', 'Maquinando', 'Hilvanando', 'Horneando',
  'Decantando', 'Urdiendo', 'Alambicando', 'Barbechando', 'Trenzando',
  'Curando', 'Leudando', 'Templando', 'Afinando', 'Devanando',
];

export function whimsyFor(jobId: string | null | undefined): string {
  const key = String(jobId ?? '');
  let hash = 0;
  for (let i = 0; i < key.length; i++) hash = ((hash << 5) - hash + key.charCodeAt(i)) | 0;
  return WHIMSY[Math.abs(hash) % WHIMSY.length];
}
