-- Tool owners leave the actor head.
--
-- chat.State.Tools maps every tool call an actor has seen to the provider
-- activity that owns it: tens of thousands of entries on a long chat (14 MB
-- of a 19 MB head), all rewritten by every checkpoint. They now live one row
-- per tool call, and a checkpoint writes only the owners that changed (as it
-- does ledger rows). owner_json is the ToolState in its actor JSON encoding;
-- the head (state_json) carries no Tools field.
CREATE TABLE chat_tools (
	chat_id    TEXT NOT NULL REFERENCES chats(chat_id) ON DELETE CASCADE,
	tool_id    TEXT NOT NULL,
	owner_json TEXT NOT NULL,
	PRIMARY KEY (chat_id, tool_id)
) WITHOUT ROWID;

-- Existing heads move their owners into the table and drop the field (a
-- null or empty Tools has no owners).
INSERT INTO chat_tools(chat_id, tool_id, owner_json)
	SELECT chats.chat_id, tools.key, tools.value
	FROM chats, json_each(chats.state_json, '$.Tools') AS tools
	WHERE json_type(chats.state_json, '$.Tools') = 'object';

UPDATE chats SET state_json = json_remove(state_json, '$.Tools')
	WHERE json_type(state_json, '$.Tools') IS NOT NULL;
