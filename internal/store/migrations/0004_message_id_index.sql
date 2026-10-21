-- Lazy chat actors: history reads of a chat that is not in memory come
-- straight from messages. A page "before message X" and a folded tool's
-- full output are addressed by stable message id, so find a row by it
-- without scanning the chat. seq is the last column, so the newest row with
-- an id comes from the index alone (and the planner prefers it to the
-- primary key's seq order). Not UNIQUE: State.Validate already rejects a
-- duplicate id, and an index build must never be the thing that fails.
CREATE INDEX messages_by_message_id ON messages(chat_id, message_id, seq);
