// Which Agent API methods this window forwards to the daemon that owns the
// addressed chat.
//
// This list mirrors `remoteAgentMethodKinds` in `cmd/workass/agent_remote_parity.go`,
// which is the authority: that table decides whether a call is remote at all
// and hands this window the machine it resolved. The five methods absent here
// are not gaps — `chat.list`, `chat.read`, `chat.diagnostics`, `chat.send` and
// `chat.cancel` keep their own renderer-native paths, because they also read or
// write this window's mirror of the remote transcript and its queue.
//
// Local-only methods (questions, hosted artifacts, rendered pages, scheduled
// messages, external work, obligations) never arrive here: the daemon rejects
// them with their own reason before any route is opened.
export const REMOTE_AGENT_CONTROL_METHODS: ReadonlySet<string> = new Set([
  'chat.create',
  'chat.subsession',
  'chat.configure',
  'chat.rename',
  'chat.settle',
  'chat.focus',
  'chat.delete',
  'spawned_work.list',
  'spawned_work.receipts',
  'agent.catalog',
  'agent.spawn',
  'agent.list',
  'agent.receipts',
  'agent.wait',
  'agent.wait_many',
  'agent.message',
  'agent.retry',
  'agent.cancel',
  'agent.decide_permission',
  'fork.create',
  'fork.list',
  'fork.run',
  'fork.wait',
  'fork.cancel',
  'fork.retry',
]);

/** The channel the owning daemon answers these on. Additive to the frozen protocol. */
export const AGENT_CONTROL_CHANNEL = 'agent:control';
