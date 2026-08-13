import { randomUUID } from 'node:crypto';

const exclusiveSessions = new Set();
let transientOAuthRefreshFailures = 0;

// --- Hostile-fixture state (all behind default-off WORKASS_CLAUDE_FIXTURE_*
// flags; when the flags are unset none of it is ever touched). ---
// One-shot latch for [fixture:oauth-once]: the marked prompt fails exactly once
// with the refresh-failure shape, so a retry of the SAME message succeeds —
// faithful to a transient refresh failure striking mid-session.
let oauthOnceMarkerFailures = 0;
// Fork-on-resume bookkeeping: transcript depth (user turns ingested) per
// provider-side session id. The real CLI's fork family (forkSession, /clear,
// conversation_reset) copies the resumed transcript under a NEW id; turns after
// the fork accrue under the forked id only, so resuming the ORIGINAL id later
// continues a transcript that is missing them.
let forkOnResumeCounter = 0;
const forkTranscriptDepths = new Map();

// Faithful to the real CLI's session files: once a session id has ingested a
// prompt its transcript exists on disk and OUTLIVES the process, so any later
// fresh `sessionId:` start with that id is refused while `resume:` succeeds.
// Enabled by WORKASS_CLAUDE_FIXTURE_PERSISTENT_SESSION_FILES=1; ids listed in
// WORKASS_CLAUDE_FIXTURE_PREEXISTING_SESSIONS (comma-separated) exist from the
// start, as if an earlier host process had written them.
const persistentSessionFiles = new Set(
  String(process.env.WORKASS_CLAUDE_FIXTURE_PREEXISTING_SESSIONS || '')
    .split(',').map((id) => id.trim()).filter(Boolean),
);

function persistentSessionFilesEnabled() {
  return process.env.WORKASS_CLAUDE_FIXTURE_PERSISTENT_SESSION_FILES === '1';
}

const transientOAuthRefreshError = 'Failed to authenticate: OAuth session expired and could not be refreshed';

function fixtureSessionId(options) {
  return options.resume || options.sessionId || 'fixture-claude-session';
}

// Claude commands surface fixture (default-off, WORKASS_CLAUDE_FIXTURE_COMMAND_
// CATALOG=1): initializationResult() reports 600 slash commands so the host's
// 512-entry clamp has 88 to count, with over-long fields and six aliases on the
// first entry so the per-string clips and the 4-alias cap are exercised too.
// The [fixture:commands-changed] and [fixture:local-command-output] prompt
// markers script the two mid-session system frames of the surface.
function commandCatalogFixtureEnabled() {
  return process.env.WORKASS_CLAUDE_FIXTURE_COMMAND_CATALOG === '1';
}

function fixtureCatalogCommands() {
  const commands = [];
  for (let i = 0; i < 600; i++) {
    commands.push({
      name: `fixture-command-${i}`,
      description: i === 0 ? `Fixture command description ${'x'.repeat(300)}` : `Fixture command ${i}`,
      argumentHint: i === 0 ? `<${'a'.repeat(120)}>` : '<args>',
      ...(i === 0 ? { aliases: ['al-0', 'al-1', 'al-2', 'al-3', 'al-4', 'al-5'] } : {}),
    });
  }
  return commands;
}

function fixtureCatalogAgents() {
  return [
    { name: 'Explore', description: 'Fixture explore agent', model: 'sonnet' },
    { name: 'Plan', description: 'Fixture plan agent' },
  ];
}

// Fixture background task, shaped like the SDK's BackgroundTaskSummary. The
// free-text fields carry a secret so the redaction law has something to fail
// on: nothing here but id/type/status may reach the wire.
function fixtureBackgroundTask(index = 0, status = 'running') {
  return {
    id: `fixture-bg-${index}`,
    type: 'shell',
    status,
    description: 'Fixture background work sk-ant-fixture-SECRET',
    command: 'curl -H "Authorization: Bearer sk-ant-fixture-SECRET" https://example.test',
  };
}

function fixtureSessionCron() {
  return {
    id: 'fixture-cron-1',
    schedule: '*/20 * * * *',
    recurring: true,
    prompt: 'Fixture wake prompt sk-ant-fixture-SECRET',
  };
}

// Marker parser: [fixture:key=value] or [fixture:key].
function fixtureMarker(text, key) {
  const withValue = new RegExp(`\\[fixture:${key}=([^\\]]*)\\]`).exec(text || '');
  if (withValue) return withValue[1];
  return (text || '').includes(`[fixture:${key}]`) ? '' : null;
}

class FixtureQuery {
  constructor({ prompt, options }) {
    this.prompt = prompt;
    this.options = options;
    this.events = [];
    this.waiters = [];
    this.closed = false;
    this.sessionId = options.resume || options.sessionId || 'fixture-claude-session';
    this.permissionMode = options.permissionMode || 'default';
    this.conversationSeen = Boolean(options.resume);
    this.transportReady = true;
    // A turn that streamed output but has not emitted its terminal result yet.
    this.openTurn = false;
    // Whether the open turn has committed anything the provider would accept as
    // an ending. Claude Code validates the last message before it reports a
    // result, so this decides which result an abort produces.
    this.turnProducedContent = false;
    // The harness's own request key: one uuid per prompt, carried on every hook
    // input until the next prompt.
    this.promptId = '';
    if (process.env.WORKASS_CLAUDE_FIXTURE_FORK_ON_RESUME === '1' && options.resume) {
      // Faithful to the real CLI's fork-on-resume family: the resumed
      // conversation continues under a NEW session id whose transcript starts
      // as a copy of the resumed one. Every SDK message from here on — init
      // included — carries the forked id, never the id that was resumed.
      const forked = `${options.resume}-fork-${++forkOnResumeCounter}`;
      forkTranscriptDepths.set(forked, forkTranscriptDepths.get(options.resume) || 0);
      this.sessionId = forked;
    }
    this.emit({
      type: 'system', subtype: 'init', session_id: this.sessionId,
      capabilities: ['fixture'],
    });
    if (process.env.WORKASS_CLAUDE_FIXTURE_FORK_ON_RESUME === '1' && options.resume) {
      // The real SDK announces the id change (SDKConversationResetMessage).
      this.emit({
        type: 'conversation_reset', new_conversation_id: this.sessionId,
        uuid: randomUUID(), session_id: this.sessionId,
      });
    }
    this.consume();
  }

  emit(message) {
    const waiter = this.waiters.shift();
    if (waiter) waiter({ value: message, done: false });
    else this.events.push(message);
  }

  async consume() {
    for await (const message of this.prompt) {
      if (this.closed) break;
      this.conversationSeen = true;
      if (persistentSessionFilesEnabled()) persistentSessionFiles.add(this.sessionId);
      if (process.env.WORKASS_CLAUDE_FIXTURE_FORK_ON_RESUME === '1') {
        forkTranscriptDepths.set(this.sessionId, (forkTranscriptDepths.get(this.sessionId) || 0) + 1);
      }
      // Also faithful: a steer carries priority "now", which makes Claude Code
      // abort the live query on the spot. If the open turn has committed
      // nothing, the conversation ends on that pushed user message with no stop
      // reason — an ending Claude Code itself rejects, so the segment closes
      // with a FAILED result carrying its own diagnostic instead of the clean
      // boundary a steer expects, and its query returns (user report
      // 2026-07-27: steering fast killed the turn and ate the direction).
      if (message.priority === 'now' && this.openTurn && !this.turnProducedContent) {
        this.openTurn = false;
        this.emit({
          ...this.result('error_during_execution'),
          stop_reason: null,
          errors: ['[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null'],
        });
        continue;
      }
      // Faithful to the real SDK: input pushed into a live turn does not
      // redirect the sampling step. The current assistant turn is closed with
      // its own terminal result and the pushed message is answered by the NEXT
      // turn, so a steer costs exactly one extra result.
      if (this.openTurn) {
        this.openTurn = false;
        this.emit(this.result('success'));
      }
      this.turnProducedContent = false;
      const text = message.message?.content?.find?.((block) => block.type === 'text')?.text || '';
      this.promptId = randomUUID();
      await this.fireHook('UserPromptSubmit', { prompt: text });
      this.emit({
        type: 'user',
        message: message.message,
        parent_tool_use_id: null,
        uuid: message.uuid,
        session_id: this.sessionId,
        origin: { kind: 'human' },
      });
      const images = (message.message?.content || []).filter((block) => block?.type === 'image');
      if (process.env.WORKASS_CLAUDE_FIXTURE_PERSISTENT_OAUTH_RESULT === '1'
          || (process.env.WORKASS_CLAUDE_FIXTURE_TRANSIENT_OAUTH_RESULT === '1'
            && transientOAuthRefreshFailures++ === 0)) {
        for (const text of ['Failed to authenticate: ', 'OAuth session expired and could not be refreshed']) {
          this.emit({
            type: 'assistant',
            message: { content: [{ type: 'text', text }] },
            parent_tool_use_id: null,
            uuid: randomUUID(),
            session_id: this.sessionId,
          });
        }
        this.emit({
          ...this.result('error_during_execution'),
          result: transientOAuthRefreshError,
          errors: [transientOAuthRefreshError],
        });
        continue;
      }
      if (text.includes('[fixture:terminal-error]')) {
        this.emit({
          ...this.result('error_during_execution'),
          result: 'Fixture terminal provider failure',
          errors: ['Fixture terminal provider failure'],
        });
        continue;
      }
      if (text.includes('[fixture:close-without-result]')) {
        this.close();
        continue;
      }
      // Mid-session OAuth expiry: unlike TRANSIENT_OAUTH (which strikes the
      // first prompt of the process) this is armed by the prompt text, so it
      // can strike AFTER successful turns. It also emits the auth_status frame
      // that brackets a refresh attempt in the real CLI — a frame type the
      // mock never produced before, carrying free text no client may see.
      if (process.env.WORKASS_CLAUDE_FIXTURE_OAUTH_EXPIRY_MARKER === '1'
          && text.includes('[fixture:oauth-once]') && oauthOnceMarkerFailures++ === 0) {
        this.emit({
          type: 'auth_status', isAuthenticating: true,
          output: ['Refreshing OAuth token sk-ant-oat01-FIXTURE-AUTH-SECRET'],
          uuid: randomUUID(), session_id: this.sessionId,
        });
        for (const chunk of ['Failed to authenticate: ', 'OAuth session expired and could not be refreshed']) {
          this.emit({
            type: 'assistant',
            message: { content: [{ type: 'text', text: chunk }] },
            parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
          });
        }
        this.emit({
          ...this.result('error_during_execution'),
          result: transientOAuthRefreshError,
          errors: [transientOAuthRefreshError],
        });
        continue;
      }
      // Transport death mid-tool-call: the CLI opened a tool_use block and the
      // process died before any tool_result or terminal result existed.
      if (process.env.WORKASS_CLAUDE_FIXTURE_TOOL_STREAM_DEATH === '1'
          && text.includes('[fixture:tool-then-close]')) {
        this.emit({
          type: 'stream_event',
          event: {
            type: 'content_block_start', index: 1,
            content_block: { type: 'tool_use', id: 'fixture-dead-tool-1', name: 'Bash', input: { command: 'sleep 999' } },
          },
          parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
        });
        this.close();
        continue;
      }
      // A terminal error result whose subtype this host has never seen — the
      // SDKResultError subtype union grows over releases.
      if (process.env.WORKASS_CLAUDE_FIXTURE_UNKNOWN_RESULTS === '1'
          && text.includes('[fixture:unknown-error-result]')) {
        this.emit({
          ...this.result('error_during_execution'),
          subtype: 'error_credits_required',
          stop_reason: null,
          errors: ['Claude AI usage limit reached: credits required to continue'],
        });
        continue;
      }
      // A NON-error result under an unknown subtype: future CLIs may close a
      // successful turn with extra advisory payloads.
      if (process.env.WORKASS_CLAUDE_FIXTURE_UNKNOWN_RESULTS === '1'
          && text.includes('[fixture:unknown-success-result]')) {
        this.emit({
          type: 'stream_event',
          event: { type: 'content_block_delta', delta: { type: 'text_delta', text: 'Fixture advisory answer' } },
          parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
        });
        this.emit({
          ...this.result('success'),
          subtype: 'success_with_advisories',
          advisories: [{ kind: 'model_fallback', from: 'claude-opus-fixture', to: 'claude-haiku-fixture' }],
        });
        continue;
      }
      // AskUserQuestion arrives through canUseTool like any other tool — this
      // fixture drives the host's question mapping end to end (the answer comes
      // back as a deny message, the only channel PermissionResult offers).
      if (text.includes('exercise question')) {
        const input = {
          questions: [{
            question: '¿A qué máquina deployamos primero?',
            header: 'Deploy target',
            multiSelect: false,
            options: [
              { label: 'El nodo de build', description: 'Canary; ya tiene el artefacto' },
              { label: 'Gold', description: 'Producción, sigue offline' },
            ],
          }],
        };
        // The real SDK opens a tool_use block for the question, and closes it with
        // a tool_result once canUseTool answers. A denial — the only channel an
        // answer has — closes it with is_error, so the row's status hangs on this.
        this.emit({
          type: 'assistant',
          message: { content: [{ type: 'tool_use', id: 'tool-question-1', name: 'AskUserQuestion', input }] },
          parent_tool_use_id: null,
          uuid: randomUUID(),
          session_id: this.sessionId,
        });
        const result = await this.options.canUseTool('AskUserQuestion', input, {
          signal: new AbortController().signal,
          toolUseID: 'tool-question-1',
          requestId: 'permission-question-1',
          title: 'AskUserQuestion',
        });
        this.emit({
          type: 'user',
          message: { content: [{
            type: 'tool_result', tool_use_id: 'tool-question-1',
            is_error: result.behavior === 'deny', content: result.message || '',
          }] },
          parent_tool_use_id: null,
          uuid: randomUUID(),
          session_id: this.sessionId,
        });
        this.emit({
          type: 'stream_event',
          event: { type: 'content_block_delta', delta: { type: 'text_delta', text: `question outcome: ${result.behavior} ${result.message || ''}` } },
          parent_tool_use_id: null,
          uuid: randomUUID(),
          session_id: this.sessionId,
        });
        this.emit(this.result('success'));
        continue;
      }
      // Same tool, but raised from inside a subagent: agentID is set, and the
      // question must go back to the parent instead of onto the user's screen.
      if (text.includes('exercise subagent question')) {
        const result = await this.options.canUseTool('AskUserQuestion', {
          questions: [{
            question: '¿Pusheo los commits pendientes?',
            header: 'Push',
            multiSelect: false,
            options: [{ label: 'Sí, pusheá' }, { label: 'No, dejalos locales' }],
          }],
        }, {
          signal: new AbortController().signal,
          toolUseID: 'tool-subagent-question-1',
          requestId: 'permission-subagent-question-1',
          agentID: 'subagent-7',
          title: 'AskUserQuestion',
        });
        this.emit({
          type: 'stream_event',
          event: { type: 'content_block_delta', delta: { type: 'text_delta', text: `subagent question outcome: ${result.behavior} ${result.message || ''}` } },
          parent_tool_use_id: null,
          uuid: randomUUID(),
          session_id: this.sessionId,
        });
        this.emit(this.result('success'));
        continue;
      }
      if (text.includes('exercise permission')) {
        const result = await this.options.canUseTool('Bash', { command: 'printf fixture' }, {
          signal: new AbortController().signal,
          toolUseID: 'tool-fixture-1',
          requestId: 'permission-fixture-1',
          title: 'Run fixture command',
          displayName: 'Run command',
        });
        if (result.behavior !== 'allow') throw new Error('fixture permission denied');
      }
      // A turn that opens and stays silent — the window in which a steer's
      // interrupt leaves the provider with nothing valid to end on.
      if (text.includes('[fixture:silent-turn]')) { this.openTurn = true; continue; }
      // Context compaction mid-turn: status(compacting) then compact_boundary,
      // exactly the frames the real CLI emits, after which the answer continues
      // inside the same turn.
      if (process.env.WORKASS_CLAUDE_FIXTURE_COMPACT_BOUNDARY === '1'
          && text.includes('[fixture:compact]')) {
        this.emit({
          type: 'system', subtype: 'status', status: 'compacting',
          uuid: randomUUID(), session_id: this.sessionId,
        });
        this.emit({
          type: 'system', subtype: 'compact_boundary',
          compact_metadata: { trigger: 'auto', pre_tokens: 168000, post_tokens: 24000, duration_ms: 1200 },
          uuid: randomUUID(), session_id: this.sessionId,
        });
      }
      // Scripted mid-session command-list replace (SDKCommandsChangedMessage):
      // full-list REPLACE semantics, then the turn answers normally.
      if (commandCatalogFixtureEnabled() && text.includes('[fixture:commands-changed]')) {
        this.emit({
          type: 'system', subtype: 'commands_changed',
          commands: [
            { name: 'changed-one', description: 'Fixture changed command', argumentHint: '' },
            { name: 'changed-two', description: 'Second fixture changed command', argumentHint: '<pr>' },
          ],
          uuid: randomUUID(), session_id: this.sessionId,
        });
      }
      // A local command's output (/usage, /cost): meant to render as assistant
      // text; dropping it leaves a picked local command with an empty turn.
      if (commandCatalogFixtureEnabled() && text.includes('[fixture:local-command-output]')) {
        this.emit({
          type: 'system', subtype: 'local_command_output',
          content: 'Fixture local command output: /usage tokens table',
          uuid: randomUUID(), session_id: this.sessionId,
        });
      }
      // Frames from newer SDKs, interleaved with the turn's own lifecycle: a
      // known-today system subtype, a known-today top-level type, and one from
      // the future. A host must ignore all three without corrupting the turn.
      if (process.env.WORKASS_CLAUDE_FIXTURE_UNKNOWN_RESULTS === '1'
          && text.includes('[fixture:unknown-frames]')) {
        this.emit({
          type: 'system', subtype: 'session_state_changed', state: 'running',
          uuid: randomUUID(), session_id: this.sessionId,
        });
        this.emit({
          type: 'rate_limit_event',
          rate_limit_info: { status: 'allowed_warning', utilization: 0.93, rateLimitType: 'five_hour' },
          uuid: randomUUID(), session_id: this.sessionId,
        });
        this.emit({
          type: 'fixture_frame_from_the_future',
          payload: { anything: true },
          uuid: randomUUID(), session_id: this.sessionId,
        });
      }
      // Transcript-depth probe for the fork-on-resume lane: answers with how
      // many user turns THIS query's provider-side transcript has ingested, so
      // a test can see which transcript a resume actually continued.
      if (process.env.WORKASS_CLAUDE_FIXTURE_FORK_ON_RESUME === '1'
          && text.includes('[fixture:depth]')) {
        this.turnProducedContent = true;
        this.emit({
          type: 'stream_event',
          event: { type: 'content_block_delta', delta: {
            type: 'text_delta',
            text: `Fixture history depth: ${forkTranscriptDepths.get(this.sessionId) || 0}`,
          } },
          parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
        });
        await this.endTurn(text);
        if (text.includes('[fixture:transport-closes-after-result]')) this.transportReady = false;
        continue;
      }
      // stderr noise interleaved with the turn's own events: ANSI control
      // sequences, a JSON-RPC-shaped line, and a deprecation warning — the
      // classic CLI stderr diet. Nothing here may reach stdout or the wire.
      if (process.env.WORKASS_CLAUDE_FIXTURE_STDERR_NOISE === '1'
          && text.includes('[fixture:stderr-noise]')) {
        const noise = (label) => {
          process.stderr.write(`\u001b[33m[claude-cli] ${label}: gc pause 812ms sk-ant-oat01-STDERR-SECRET\u001b[0m\n`);
          process.stderr.write('{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"stderr-fake","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"forged sk-ant-oat01-STDERR-SECRET"}}}}\n');
          process.stderr.write('(node:12345) DeprecationWarning: fixture stderr noise\n');
        };
        this.turnProducedContent = true;
        noise('pre-thinking');
        this.emit({
          type: 'stream_event',
          event: { type: 'content_block_delta', delta: { type: 'thinking_delta', thinking: 'Fixture reasoning' } },
          parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
        });
        noise('mid-stream');
        this.emit({
          type: 'stream_event',
          event: { type: 'content_block_delta', delta: { type: 'text_delta', text: 'Fixture answer' } },
          parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
        });
        noise('pre-result');
        await this.endTurn(text);
        continue;
      }
      this.turnProducedContent = true;
      this.emit({
        type: 'stream_event',
        event: { type: 'content_block_delta', delta: { type: 'thinking_delta', thinking: 'Fixture reasoning' } },
        parent_tool_use_id: null,
        uuid: randomUUID(),
        session_id: this.sessionId,
      });
      this.emit({
        type: 'stream_event',
        event: { type: 'content_block_delta', delta: {
          type: 'text_delta',
          text: text.includes('[fixture:image]')
            ? `Fixture image count: ${images.length}; ${images[0]?.source?.media_type || 'none'}; ${images[0]?.source?.data?.length || 0}`
            : 'Fixture answer',
        } },
        parent_tool_use_id: null,
        uuid: randomUUID(),
        session_id: this.sessionId,
      });
      if (text.includes('keep running')) { this.openTurn = true; continue; }
      // A hook firing from inside a subagent carries agent_id and is NOT this
      // session's turn ending. Emitted before the real Stop so a consumer that
      // fails to filter it settles the turn early and the test catches it.
      if (fixtureMarker(text, 'subagent-hook') !== null) {
        await this.fireHook('Stop', {
          stop_hook_active: false, agent_id: 'fixture-subagent-1', agent_type: 'Explore',
          background_tasks: [fixtureBackgroundTask(9)], session_crons: [],
        });
      }
      const notify = fixtureMarker(text, 'notify');
      if (notify !== null) {
        await this.fireHook('Notification', {
          notification_type: notify || 'permission_prompt',
          message: 'Fixture notification sk-ant-fixture-SECRET',
        }, notify || 'permission_prompt');
      }
      if (fixtureMarker(text, 'stop-failure') !== null) {
        await this.fireHook('StopFailure', {
          error: 'rate_limit', error_details: 'Fixture rate limit sk-ant-fixture-SECRET',
        }, 'rate_limit');
        this.emit(this.result('error_during_execution', { terminal_reason: 'api_error' }));
        continue;
      }
      await this.endTurn(text);
      if (fixtureMarker(text, 'harness-turn') !== null) await this.harnessTurn();
      if (text.includes('[fixture:transport-closes-after-result]')) {
        this.transportReady = false;
      }
    }
  }

  result(subtype, extra = {}) {
    return {
      type: 'result', subtype, is_error: subtype !== 'success', result: '', stop_reason: 'end_turn',
      terminal_reason: subtype === 'success' ? 'completed' : 'api_error',
      duration_ms: 1, duration_api_ms: 1, num_turns: 1, total_cost_usd: 0,
      usage: { input_tokens: 1, output_tokens: 1 }, modelUsage: {}, permission_denials: [],
      uuid: randomUUID(), session_id: this.sessionId, ...extra,
    };
  }

  // Faithful to the SDK's hook dispatch: every matcher group whose matcher is
  // absent, '*' or exactly the event's filter value runs its callbacks. Hook
  // return values are ignored here — this lane's hooks are observers.
  async fireHook(event, input, filterValue = '') {
    const groups = this.options?.hooks?.[event];
    if (!Array.isArray(groups)) return;
    const base = {
      session_id: this.sessionId,
      transcript_path: `/tmp/fixture-transcript-${this.sessionId}.jsonl`,
      cwd: this.options?.cwd || '/tmp',
      prompt_id: this.promptId,
      permission_mode: this.permissionMode,
      hook_event_name: event,
      ...input,
    };
    for (const group of groups) {
      const matcher = String(group?.matcher ?? '').trim();
      if (matcher && matcher !== '*' && matcher !== filterValue) continue;
      for (const hook of Array.isArray(group?.hooks) ? group.hooks : []) {
        try {
          await hook(base, undefined, { signal: new AbortController().signal });
        } catch (error) {
          // A throwing hook must not take the turn down with it.
          process.stderr.write(`fixture hook ${event} threw: ${error?.message}\n`);
        }
      }
    }
  }

  // The Stop hook always precedes the terminal result — the ordering the daemon
  // relies on to have harness evidence in hand before job:end.
  async endTurn(text, extra = {}) {
    const many = fixtureMarker(text, 'bg-many');
    const backgroundTasks = fixtureMarker(text, 'bg-undefined') !== null
      ? undefined
      : many !== null
        ? Array.from({ length: 25 }, (_, i) => fixtureBackgroundTask(i))
        : fixtureMarker(text, 'bg-task') !== null
          ? [fixtureBackgroundTask(0, fixtureMarker(text, 'bg-task') || 'running')]
          : [];
    const sessionCrons = fixtureMarker(text, 'cron') !== null ? [fixtureSessionCron()] : [];
    await this.fireHook('Stop', {
      stop_hook_active: false,
      last_assistant_message: 'Fixture answer',
      ...(backgroundTasks === undefined ? {} : { background_tasks: backgroundTasks }),
      session_crons: sessionCrons,
    });
    this.emit(this.result('success', extra));
  }

  // A turn nobody asked for: the harness wakes the session when background work
  // settles. This is the shape that produced the 76-minute false park.
  async harnessTurn() {
    this.emit({
      type: 'system', subtype: 'task_notification', session_id: this.sessionId,
      task_id: 'fixture-bg-0', uuid: randomUUID(),
    });
    this.promptId = randomUUID();
    await this.fireHook('UserPromptSubmit', { prompt: 'Background task fixture-bg-0 completed' });
    this.emit({
      type: 'system', subtype: 'init', session_id: this.sessionId, capabilities: ['fixture'],
    });
    this.emit({
      type: 'stream_event',
      event: { type: 'content_block_delta', delta: { type: 'text_delta', text: 'Fixture harness-born answer' } },
      parent_tool_use_id: null, uuid: randomUUID(), session_id: this.sessionId,
    });
    await this.fireHook('Stop', {
      stop_hook_active: false,
      last_assistant_message: 'Fixture harness-born answer',
      background_tasks: [],
      session_crons: [],
    });
    this.emit(this.result('success', { origin: { kind: 'task-notification' } }));
  }

  initializationResult() {
    if (process.env.WORKASS_CLAUDE_FIXTURE_AUTH_ERROR === '1') {
      return Promise.reject(new Error('Authentication required: run claude auth login'));
    }
    if (process.env.WORKASS_CLAUDE_FIXTURE_MISSING_RESUME === '1' && this.options.resume) {
      return Promise.reject(new Error(`No conversation found with session ID: ${this.sessionId}`));
    }
    return Promise.resolve({
      account: { email: 'fixture@example.test', subscriptionType: 'max' },
      ...(commandCatalogFixtureEnabled()
        ? {
          commands: fixtureCatalogCommands(), agents: fixtureCatalogAgents(),
          output_style: 'default', available_output_styles: ['default', 'explanatory'],
        }
        : { commands: [], agents: [], output_style: 'default', available_output_styles: [] }),
      models: [
        {
          value: 'default', resolvedModel: 'claude-opus-fixture', displayName: 'Default (recommended)',
          description: 'Fixture default', supportsEffort: true,
          supportedEffortLevels: ['low', 'medium', 'high', 'xhigh', 'max'],
        },
        {
          value: 'claude-opus-fixture', resolvedModel: 'claude-opus-fixture', displayName: 'Claude Opus Fixture',
          description: 'Fixture model', supportsEffort: true,
          supportedEffortLevels: ['low', 'medium', 'high', 'xhigh', 'max'],
        },
        { value: 'claude-haiku-fixture', resolvedModel: 'claude-haiku-fixture', displayName: 'Claude Haiku Fixture' },
      ],
    });
  }

  interrupt() {
    if (process.env.WORKASS_CLAUDE_FIXTURE_INTERRUPT_HANG === '1') {
      return new Promise(() => {});
    }
    this.emit(this.result('error_during_execution'));
    return Promise.resolve({ still_queued: [] });
  }

  setModel() {
    if (!this.transportReady) {
      return Promise.reject(new Error('ProcessTransport is not ready for writing'));
    }
    if (!this.conversationSeen) {
      return Promise.reject(new Error(`No conversation found with session ID: ${this.sessionId}`));
    }
    return Promise.resolve();
  }
  setPermissionMode(value) {
    if (!this.transportReady) {
      return Promise.reject(new Error('ProcessTransport is not ready for writing'));
    }
    if (value === 'bypassPermissions' && this.options.allowDangerouslySkipPermissions !== true) {
      return Promise.reject(new Error('Cannot set permission mode to bypassPermissions because the session was not launched with --dangerously-skip-permissions'));
    }
    if (!this.conversationSeen) {
      return Promise.reject(new Error(`No conversation found with session ID: ${this.sessionId}`));
    }
    this.permissionMode = value;
    return Promise.resolve();
  }
  setMaxThinkingTokens() { return Promise.resolve(); }
  close() {
    this.closed = true;
    for (const waiter of this.waiters.splice(0)) waiter({ value: undefined, done: true });
    const release = () => exclusiveSessions.delete(this.sessionId);
    if (process.env.WORKASS_CLAUDE_FIXTURE_EXCLUSIVE_SESSION === '1') {
      const delay = Number(process.env.WORKASS_CLAUDE_FIXTURE_CLOSE_DELAY_MS || 30);
      return new Promise((resolve) => setTimeout(() => { release(); resolve(); }, delay));
    }
    release();
    return Promise.resolve();
  }

  [Symbol.asyncIterator]() { return this; }
  next() {
    if (this.events.length) return Promise.resolve({ value: this.events.shift(), done: false });
    if (this.closed) return Promise.resolve({ value: undefined, done: true });
    return new Promise((resolve) => this.waiters.push(resolve));
  }
}

export function query(input) {
	if (process.env.WORKASS_CLAUDE_FIXTURE_REQUIRE_CANONICAL_MCP === '1') {
		const server = input.options?.mcpServers?.['workass-agent'];
		if (server?.type !== 'http' || server?.headers?.Authorization !== 'Bearer fixture-owner') {
			throw new Error('Claude SDK MCP descriptor was not normalized');
		}
	}
  if (input.options?.permissionMode === 'bypassPermissions'
      && input.options?.allowDangerouslySkipPermissions !== true) {
    throw new Error('Cannot launch bypassPermissions without allowDangerouslySkipPermissions');
  }
  const sessionId = fixtureSessionId(input.options || {});
  if (process.env.WORKASS_CLAUDE_FIXTURE_EXCLUSIVE_SESSION === '1') {
    if (exclusiveSessions.has(sessionId)) {
      throw new Error(`Session ID ${sessionId} is already in use.`);
    }
    exclusiveSessions.add(sessionId);
  }
  if (persistentSessionFilesEnabled()
      && !input.options?.resume && input.options?.sessionId
      && persistentSessionFiles.has(sessionId)) {
    throw new Error(`Session ID ${sessionId} is already in use.`);
  }
  return new FixtureQuery(input);
}
