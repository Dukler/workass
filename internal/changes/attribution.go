package changes

import (
	"path/filepath"
	"sort"
	"strings"
)

// activityIndex is what the chats say, resolved onto repos: which repos they
// touched and when, which chats touched each repo, and for each file an agent
// edited, the chat that edited it last.
type activityIndex struct {
	repos     map[string]int64
	repoChats map[string]map[string]struct{}
	files     map[string]map[string]editRef
	chats     map[string]ChatActivity
}

type editRef struct {
	chatID string
	at     int64
}

// buildIndex resolves chat folders and edited files to their repos. It only
// stats the filesystem; it never starts git.
func buildIndex(finder *rootFinder, activity []ChatActivity) activityIndex {
	index := activityIndex{repos: map[string]int64{}, repoChats: map[string]map[string]struct{}{}, files: map[string]map[string]editRef{}, chats: map[string]ChatActivity{}}
	touch := func(root, chatID string, at int64) {
		if root == "" {
			return
		}
		chats := index.repoChats[root]
		if chats == nil {
			chats = map[string]struct{}{}
			index.repoChats[root] = chats
		}
		chats[chatID] = struct{}{}
		if at > index.repos[root] {
			index.repos[root] = at
		} else if _, ok := index.repos[root]; !ok {
			index.repos[root] = at
		}
	}
	for _, chat := range activity {
		chatID := strings.TrimSpace(chat.ChatID)
		if chatID == "" {
			continue
		}
		summary := chat
		summary.Edits = nil
		index.chats[chatID] = summary
		if cwd := strings.TrimSpace(chat.CWD); cwd != "" {
			touch(finder.rootOf(cwd), chatID, chat.LastActivityAt)
		}
		for _, edit := range chat.Edits {
			if !filepath.IsAbs(edit.Path) {
				continue
			}
			path := finder.canonicalPath(edit.Path)
			root := finder.rootOf(filepath.Dir(path))
			if root == "" {
				continue
			}
			rel, ok := relInside(root, path)
			if !ok {
				continue
			}
			touch(root, chatID, edit.At)
			files := index.files[root]
			if files == nil {
				files = map[string]editRef{}
				index.files[root] = files
			}
			key := pathKey(rel)
			if current, seen := files[key]; !seen || edit.At > current.at || (edit.At == current.at && chatID < current.chatID) {
				files[key] = editRef{chatID: chatID, at: edit.At}
			}
		}
	}
	return index
}

// chatsOf lists the chats that work in or edited the repo, sorted.
func (index activityIndex) chatsOf(root string) []string {
	out := make([]string, 0, len(index.repoChats[root]))
	for chatID := range index.repoChats[root] {
		out = append(out, chatID)
	}
	sort.Strings(out)
	return out
}
