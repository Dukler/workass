// One exact chat can have at most one transactional workspace move in flight.
// Send/attachment initialization awaits this same promise, so it cannot create
// a provider session or dispatch a turn between daemon commit and renderer
// adoption of the target cwd.
export class WorkspaceMoveGate {
  private moves = new Map<string, Promise<boolean>>();

  current(chatId: string): Promise<boolean> | undefined {
    return this.moves.get(chatId);
  }

  run(chatId: string, operation: () => Promise<boolean>): Promise<boolean> {
    const existing = this.moves.get(chatId);
    if (existing) return existing;
    let move!: Promise<boolean>;
    move = operation().finally(() => {
      if (this.moves.get(chatId) === move) this.moves.delete(chatId);
    });
    this.moves.set(chatId, move);
    return move;
  }
}

export function workspaceMoveAccepted(result: {
  sessionId?: string;
  error?: string;
  workspaceCommitted?: boolean;
  workspaceRebound?: boolean;
  workspaceRevision?: number;
} | null | undefined, expectedRevision = 0): boolean {
  return !!result
    && !result.error
    && result.workspaceCommitted === true
    && result.workspaceRebound === true
    && result.sessionId === ''
    && Number.isInteger(result.workspaceRevision)
    && (result.workspaceRevision ?? 0) === expectedRevision + 1;
}

export function workspaceRebindSupported(meta: { workspaceRebindMode?: string } | null | undefined): boolean {
  return meta?.workspaceRebindMode === 'transactional-v1';
}
