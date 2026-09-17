package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// This file is WS3 of issue #7: the append-only history behind the cache.
//
// The cache answers "what does this page say now"; the history answers "what
// have I not seen yet". They are separate on purpose. A cached listing is
// overwritten by the next fetch, whereas a notification that has scrolled off
// the site can never be fetched again, so history rows are appended and never
// pruned.
//
// Only a live fetch feeds the history. A cache hit read nothing from the site
// and therefore learned nothing new; appending from it would do work to
// discover that every row is a duplicate.

// appendNotifications records a live listing. The returned count is how many
// rows were genuinely new — the store deduplicates by (account, tab, id) and
// counts a repeat inside one batch once, which matters because the site
// repeats items across an overlapping page boundary.
func (c *serviceCache) appendNotifications(ctx context.Context, tab xiaohongshu.NotificationTab, items []xiaohongshu.NotificationItem) int {
	if !c.enabled || len(items) == 0 {
		return 0
	}
	account := c.accountID()
	if account == "" {
		return 0
	}

	records := make([]store.NotificationRecord, 0, len(items))
	for _, item := range items {
		if item.ID == "" {
			// Without an id there is no identity, so a re-fetch could not
			// recognise the row as one it already has.
			continue
		}
		payload, err := json.Marshal(item)
		if err != nil {
			logrus.Warnf("history: marshalling notification %s failed: %v", item.ID, err)
			continue
		}
		records = append(records, store.NotificationRecord{
			Tab:            string(tab),
			NotificationID: item.ID,
			FromUserID:     item.From.UserID,
			NoteID:         item.FeedID,
			CommentID:      item.CommentID,
			Payload:        payload,
		})
	}
	if len(records) == 0 {
		return 0
	}

	added, err := c.store.AppendNotifications(ctx, account, records)
	if err != nil {
		// History is a side effect of a read that has already succeeded. It
		// must never turn that read into an error.
		logrus.Warnf("history: appending %d notifications failed: %v", len(records), err)
		return 0
	}
	if added > 0 {
		logrus.Debugf("history: %d new notifications in %s", added, tab)
	}
	return added
}

// appendComments records the comments of a live detail fetch.
func (c *serviceCache) appendComments(ctx context.Context, noteID string, comments []xiaohongshu.Comment) int {
	if !c.enabled || noteID == "" || len(comments) == 0 {
		return 0
	}
	account := c.accountID()
	if account == "" {
		return 0
	}

	records := flattenComments(noteID, "", comments)
	if len(records) == 0 {
		return 0
	}

	added, err := c.store.AppendComments(ctx, account, records)
	if err != nil {
		logrus.Warnf("history: appending %d comments failed: %v", len(records), err)
		return 0
	}
	if added > 0 {
		logrus.Debugf("history: %d new comments on %s", added, noteID)
	}
	return added
}

// flattenComments turns a comment tree into one row per comment, replies
// included, each carrying its parent's id.
//
// The payload has SubComments stripped, so a reply is stored once — as its own
// row — rather than once on its own and again inside its parent. The tree is
// never rebuilt from these rows; the cached document serves the tree, and the
// rows exist only to answer "what is new".
func flattenComments(noteID, parentID string, comments []xiaohongshu.Comment) []store.CommentRecord {
	var out []store.CommentRecord

	for _, comment := range comments {
		children := comment.SubComments

		if comment.ID != "" {
			flat := comment
			flat.SubComments = nil

			payload, err := json.Marshal(flat)
			if err != nil {
				logrus.Warnf("history: marshalling comment %s failed: %v", comment.ID, err)
			} else {
				// The site reports the note id on the comment itself; fall
				// back to the note we were reading when it does not.
				id := comment.NoteID
				if id == "" {
					id = noteID
				}
				out = append(out, store.CommentRecord{
					NoteID:    id,
					CommentID: comment.ID,
					ParentID:  parentID,
					AuthorID:  comment.UserInfo.UserID,
					Payload:   payload,
				})
			}
		}

		if len(children) > 0 {
			out = append(out, flattenComments(noteID, comment.ID, children)...)
		}
	}
	return out
}

// errNoHistoryStore is what since_cursor produces without a store.
//
// It is an error rather than a silently ignored argument: a caller that asked
// for "only what is new" and received a full listing would have no way to tell
// the difference, and would report everything it received as new.
var errNoHistoryStore = fmt.Errorf(
	"since_cursor 需要持久化存储：请设置 XHS_DATABASE_URL 后重试（未配置时没有历史记录可供比对）")

// errAccountUnknown is what since_cursor produces when the store is configured
// but nobody has been observed using it yet.
var errAccountUnknown = fmt.Errorf(
	"since_cursor 暂不可用：还没有识别出当前账号（通常是未登录），历史记录按账号存放")

// notificationSinceStart is the cursor a caller passes on its first sync.
//
// Cursors are opaque by contract, so there is no value an agent could invent
// that means "from the beginning" — and an empty since_cursor already means
// "no history view, just the listing". This sentinel is ours, translated to
// the store's empty cursor here and never handed back out.
const notificationSinceStart = "start"

// defaultNotificationSinceLimit bounds an unbounded request, matching the
// listing's own default of 20. A caller that receives a full page and wants
// more calls again with the cursor it just received.
const defaultNotificationSinceLimit = 20

// notificationsSince reads the history a caller has not seen, and returns the
// cursor to hand back next time.
//
// The next cursor is the last row returned, or the cursor that came in when
// nothing is new — so a caller that polls an idle inbox keeps handing back the
// same value rather than rewinding to the start of the stream.
func (c *serviceCache) notificationsSince(
	ctx context.Context, tab xiaohongshu.NotificationTab, after string, limit int,
) (items []xiaohongshu.NotificationItem, next string, err error) {
	if !c.enabled {
		return nil, "", errNoHistoryStore
	}
	account := c.accountID()
	if account == "" {
		return nil, "", errAccountUnknown
	}

	from := after
	if from == notificationSinceStart {
		from = ""
	}
	if limit <= 0 {
		limit = defaultNotificationSinceLimit
	}

	rows, err := c.store.NotificationsSince(ctx, account, string(tab), store.Cursor(from), limit)
	if err != nil {
		return nil, "", fmt.Errorf("读取通知历史失败: %w", err)
	}

	// With nothing new the caller's own cursor comes back, sentinel included:
	// an empty stream has no cursor to offer, and handing back "" would flip
	// the next call out of the history view entirely.
	next = after
	items = make([]xiaohongshu.NotificationItem, 0, len(rows))
	for _, row := range rows {
		var item xiaohongshu.NotificationItem
		if err := json.Unmarshal(row.Payload, &item); err != nil {
			// A row that no longer decodes is skipped rather than fatal, but
			// the cursor still advances past it: it will never decode either.
			logrus.Warnf("history: stored notification %s no longer decodes: %v", row.Cursor, err)
			next = string(row.Cursor)
			continue
		}
		items = append(items, item)
		next = string(row.Cursor)
	}
	return items, next, nil
}
