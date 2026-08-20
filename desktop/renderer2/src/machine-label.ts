// Where a conversation lives, in one slot: `builder/workass`.
//
// Approved 2026-07-26 (desktop/docs/mocks/machines/list.html). The machine is a
// prefix on the project the sidebar already shows — not a new element, not a
// group header, not a colour. Two rules the mock draws and this module owns:
//
//   · The local machine writes nothing. Absence means "here", like a relative
//     path, so with one machine the list is pixel-identical to before and the
//     common row pays no text to say what you already knew.
//   · When the width runs out the MACHINE clamps, never the project. Ordinary
//     text-overflow eats the tail — which is the project, the half you were
//     looking for. So they are two spans, and only the first one is capped.

export interface MachineWhere {
  /** '' for the local machine. Renders dimmer than the project. */
  machine: string;
  project: string;
  /** Everything, for the tooltip the row already has. */
  full: string;
}

export interface RemoteMachineBadge {
  machine: string;
  initial: string;
  title: string;
}

/**
 * Build the two halves of the "where" slot.
 *
 * `machineId` empty (or equal to `localMachineId`) means local, and the machine
 * half comes back empty. A machine we know by id but not by name falls back to
 * a short id rather than printing nothing: "some machine I have not named" is
 * still more than "here".
 */
export function machineWhere(
  project: string,
  machineId?: string | null,
  names?: Readonly<Record<string, string>>,
  localMachineId?: string | null,
): MachineWhere {
  const projectLabel = String(project ?? '').trim();
  const id = String(machineId ?? '').trim();
  const local = String(localMachineId ?? '').trim();
  if (!id || (local && id === local)) return { machine: '', project: projectLabel, full: projectLabel };
  const machine = String(names?.[id] ?? '').trim() || shortMachineId(id);
  return { machine, project: projectLabel, full: machine + '/' + projectLabel };
}

/**
 * A machine with no display name yet — seen by beacon, not yet probed — still
 * needs something a human can tell apart from the next one. The tail of the id
 * is random, so its last six characters distinguish machines better than its
 * head, which is a constant prefix.
 */
export function shortMachineId(machineId: string): string {
  const id = String(machineId ?? '').trim();
  const body = id.startsWith('m-') ? id.slice(2) : id;
  if (!body) return '?';
  return body.length <= 6 ? body : body.slice(-6);
}

/** Compact ownership mark for project-picking surfaces where `machine/project`
 * would consume the whole row. Local projects intentionally return null. */
export function remoteMachineBadge(
  machineId?: string | null,
  names?: Readonly<Record<string, string>>,
  localMachineId?: string | null,
): RemoteMachineBadge | null {
  const id = String(machineId ?? '').trim();
  const local = String(localMachineId ?? '').trim();
  if (!id || (local && id === local)) return null;
  const machine = String(names?.[id] ?? '').trim() || shortMachineId(id);
  const initial = Array.from(machine).find((character) => /[\p{L}\p{N}]/u.test(character))?.toLocaleUpperCase() ?? '?';
  return { machine, initial, title: `Proyecto remoto en ${machine}` };
}
