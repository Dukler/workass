package main

import (
	"os"

	"workass/internal/wire"
)

// registerArchiveHandlers preserves the frozen renderer wire methods while the
// actor ledger remains the only transcript store. archive-append is therefore
// an acknowledgement, never a second writer; archive-load is a read-only full
// history projection used when a bounded session snapshot is opened.
func registerArchiveHandlers(hub *wire.Hub, _ *daemonState, runtimes ...*providerChatRuntime) {
	var providerChats *providerChatRuntime
	if len(runtimes) > 0 {
		providerChats = runtimes[0]
	}
	hub.Register("chat:archive-append", func(args []any) (any, error) {
		arg := firstMapArg(args)
		tabID := fieldString(arg, "tabId")
		if tabID == "" || len(sliceArg(arg["messages"])) == 0 {
			return false, nil
		}
		if providerChats == nil {
			return false, os.ErrInvalid
		}
		if _, found, err := providerChats.actorByTab(tabID); err != nil {
			return false, err
		} else if found {
			return true, nil
		}
		return false, os.ErrNotExist
	})
	hub.Register("chat:archive-load", func(args []any) (any, error) {
		if providerChats == nil {
			return nil, os.ErrInvalid
		}
		if messages, found, err := providerChats.ProjectArchiveByTab(stringArg(args, 0)); err != nil {
			return nil, err
		} else if found {
			return messages, nil
		}
		return []any{}, nil
	})
}
