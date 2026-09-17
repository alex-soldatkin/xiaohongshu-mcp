package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store/memstore"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// WS3: the history log and since_cursor. Everything here runs against memstore
// with the runHook standing in for the browser, so there is no database and no
// Chrome anywhere.

func notification(id, comment string) xiaohongshu.NotificationItem {
	return xiaohongshu.NotificationItem{
		ID:        id,
		Type:      "comment",
		CommentID: comment,
		FeedID:    "note-1",
		From:      xiaohongshu.NotificationUser{UserID: "u-" + id},
	}
}

func TestLiveListingFeedsTheHistoryLog(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")

	calls := 0
	svc.runHook = countingRun(&calls)

	ctx := context.Background()
	live := &xiaohongshu.NotificationList{
		Tab:   xiaohongshu.TabMentions,
		Items: []xiaohongshu.NotificationItem{notification("n1", "c1"), notification("n2", "c2")},
	}
	svc.cache.appendNotifications(ctx, xiaohongshu.TabMentions, live.Items)

	rows, err := st.NotificationsSince(ctx, "acct-1", string(xiaohongshu.TabMentions), "", 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	var first xiaohongshu.NotificationItem
	require.NoError(t, json.Unmarshal(rows[0].Payload, &first))
	assert.Equal(t, "n1", first.ID)
	assert.Equal(t, "c1", first.CommentID)

	// The site repeats items across an overlapping page boundary. A re-fetch
	// must add only what is genuinely new, and must not move the cursor of a
	// row that is already there — otherwise every sync would resurface
	// everything.
	added := svc.cache.appendNotifications(ctx, xiaohongshu.TabMentions,
		[]xiaohongshu.NotificationItem{notification("n2", "c2"), notification("n3", "c3")})
	assert.Equal(t, 1, added)

	rows2, err := st.NotificationsSince(ctx, "acct-1", string(xiaohongshu.TabMentions), "", 0)
	require.NoError(t, err)
	require.Len(t, rows2, 3)
	assert.Equal(t, rows[0].Cursor, rows2[0].Cursor, "a re-fetched row keeps its cursor")
	assert.Equal(t, rows[1].Cursor, rows2[1].Cursor)
}

func TestHistoryIsSilentWithoutAStoreOrAnAccount(t *testing.T) {
	clock := newTestClock()
	ctx := context.Background()
	items := []xiaohongshu.NotificationItem{notification("n1", "c1")}

	// No store: the counting fake proves nothing is called at all.
	fake := &fakeStore{}
	disabled := newCacheTestService(t, fake, clock)
	disabled.cache.enabled = false
	assert.Zero(t, disabled.cache.appendNotifications(ctx, xiaohongshu.TabMentions, items))
	assert.Zero(t, disabled.cache.appendComments(ctx, "note-1", []xiaohongshu.Comment{{ID: "c1"}}))
	assert.Equal(t, 0, fake.callCount())

	// A store but no account yet: history is keyed by account, so there is
	// nowhere to put the rows and nothing may be guessed.
	unknown := newCacheTestService(t, memstore.New(), clock)
	assert.Zero(t, unknown.cache.appendNotifications(ctx, xiaohongshu.TabMentions, items))
	assert.Zero(t, unknown.cache.appendComments(ctx, "note-1", []xiaohongshu.Comment{{ID: "c1"}}))
}

func TestFlattenCommentsStoresEachReplyOnce(t *testing.T) {
	tree := []xiaohongshu.Comment{
		{
			ID:       "c1",
			NoteID:   "note-1",
			Content:  "top",
			UserInfo: xiaohongshu.User{UserID: "u1"},
			SubComments: []xiaohongshu.Comment{
				{ID: "c1a", Content: "reply", UserInfo: xiaohongshu.User{UserID: "u2"}},
				{
					ID:          "c1b",
					Content:     "reply with its own",
					SubComments: []xiaohongshu.Comment{{ID: "c1b1", Content: "deep"}},
				},
			},
		},
		{ID: "", Content: "no id, no identity"},
		{ID: "c2", Content: "another top"},
	}

	records := flattenComments("note-1", "", tree)
	require.Len(t, records, 5, "every comment and reply is its own row; the one without an id is dropped")

	byID := map[string]store.CommentRecord{}
	for _, r := range records {
		byID[r.CommentID] = r
		assert.Equal(t, "note-1", r.NoteID, "a reply inherits the note it was read from")

		var payload xiaohongshu.Comment
		require.NoError(t, json.Unmarshal(r.Payload, &payload))
		assert.Empty(t, payload.SubComments,
			"replies are stripped from the payload so each is stored exactly once")
	}

	assert.Empty(t, byID["c1"].ParentID)
	assert.Equal(t, "c1", byID["c1a"].ParentID)
	assert.Equal(t, "c1", byID["c1b"].ParentID)
	assert.Equal(t, "c1b", byID["c1b1"].ParentID, "nesting is recorded, not flattened away")
	assert.Equal(t, "u2", byID["c1a"].AuthorID)
}

// TestSinceCursorReturnsOnlyWhatIsNew walks the whole loop an agent walks:
// bootstrap with "start", keep the cursor, poll again. The listing itself is
// served from the cache throughout, so the incremental view costs no browser
// trip at all — which is the point of recording history under the cache rather
// than beside it.
func TestSinceCursorReturnsOnlyWhatIsNew(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)

	ctx := context.Background()
	listing := &xiaohongshu.NotificationList{
		Tab:   xiaohongshu.TabMentions,
		Items: []xiaohongshu.NotificationItem{notification("n1", "c1"), notification("n2", "c2")},
	}
	svc.cache.put(ctx, store.KindNotifications, string(xiaohongshu.TabMentions), listing,
		notificationListMeta{Limit: 20}, clock.Now())
	svc.cache.appendNotifications(ctx, xiaohongshu.TabMentions, listing.Items)

	// Without since_cursor the response is the listing, and carries no history
	// block at all — the shape an existing caller already knows.
	plain, err := svc.ListNotifications(ctx, "mentions", 20, "")
	require.NoError(t, err)
	assert.Nil(t, plain.History)
	require.Len(t, plain.Items, 2)

	first, err := svc.ListNotifications(ctx, "mentions", 20, notificationSinceStart)
	require.NoError(t, err)
	require.NotNil(t, first.History)
	assert.Equal(t, 2, first.History.New)
	require.Len(t, first.Items, 2)
	assert.Equal(t, "n1", first.Items[0].ID)
	assert.NotEmpty(t, first.History.NextCursor)
	assert.Equal(t, notificationSinceStart, first.History.SinceCursor)

	// A later refresh learns one more notification.
	svc.cache.appendNotifications(ctx, xiaohongshu.TabMentions,
		[]xiaohongshu.NotificationItem{notification("n2", "c2"), notification("n3", "c3")})

	second, err := svc.ListNotifications(ctx, "mentions", 20, first.History.NextCursor)
	require.NoError(t, err)
	require.NotNil(t, second.History)
	assert.Equal(t, 1, second.History.New, "only what the caller has not seen")
	require.Len(t, second.Items, 1)
	assert.Equal(t, "n3", second.Items[0].ID)
	assert.Greater(t, second.History.NextCursor, first.History.NextCursor,
		"cursors order lexicographically by byte value, as the contract requires")

	// An idle inbox returns nothing and hands the same cursor back, rather
	// than rewinding to the start of the stream.
	third, err := svc.ListNotifications(ctx, "mentions", 20, second.History.NextCursor)
	require.NoError(t, err)
	require.NotNil(t, third.History)
	assert.Equal(t, 0, third.History.New)
	assert.Empty(t, third.Items)
	assert.Equal(t, second.History.NextCursor, third.History.NextCursor)

	// The cached listing document is untouched by any of this: the history
	// view is a copy, not a mutation of what the store holds.
	doc, err := st.GetDoc(ctx, "acct-1", store.KindNotifications, string(xiaohongshu.TabMentions))
	require.NoError(t, err)
	var stored xiaohongshu.NotificationList
	require.NoError(t, json.Unmarshal(doc.Payload, &stored))
	require.Len(t, stored.Items, 2)
}

// The history page is bounded even when the caller states no limit, so a
// cursor from months ago cannot return the entire log in one response.
func TestSinceCursorPageIsBounded(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)

	ctx := context.Background()
	var items []xiaohongshu.NotificationItem
	for i := 0; i < defaultNotificationSinceLimit+5; i++ {
		items = append(items, notification(fmt.Sprintf("n%02d", i), fmt.Sprintf("c%02d", i)))
	}
	svc.cache.appendNotifications(ctx, xiaohongshu.TabMentions, items)
	svc.cache.put(ctx, store.KindNotifications, string(xiaohongshu.TabMentions),
		&xiaohongshu.NotificationList{Tab: xiaohongshu.TabMentions},
		notificationListMeta{Limit: 0}, clock.Now())

	got, err := svc.ListNotifications(ctx, "mentions", 0, notificationSinceStart)
	require.NoError(t, err)
	require.NotNil(t, got.History)
	assert.Equal(t, defaultNotificationSinceLimit, got.History.New)

	rest, err := svc.ListNotifications(ctx, "mentions", 0, got.History.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, 5, rest.History.New, "the rest arrives on the next call, from the cursor just given")
}

// TestSinceCursorWithoutAStoreIsAnError pins the refusal. Ignoring the
// argument would hand the caller a full listing it would then report as
// entirely new.
func TestSinceCursorWithoutAStoreIsAnError(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, store.Nop{}, clock)
	require.False(t, svc.cache.enabled)

	// failingRun proves the refusal happens before the browser, so a bad
	// argument costs neither a page load nor a slice of the read budget.
	svc.runHook = failingRun(t)

	_, err := svc.ListNotifications(context.Background(), "mentions", 20, "start")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "XHS_DATABASE_URL")
}

// With the store configured but nobody identified yet, the history view has no
// account to read under. The check sits after the listing rather than before
// it, because the listing is what observes the account in the first place: on a
// fresh database the very first sync can succeed this way.
func TestSinceCursorNeedsAKnownAccount(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	// Account deliberately unknown: not logged in, or nothing read yet.

	_, _, err := svc.cache.notificationsSince(context.Background(), xiaohongshu.TabMentions, notificationSinceStart, 20)
	require.ErrorIs(t, err, errAccountUnknown)
}

func TestPruneSweepsNoteSourcesToo(t *testing.T) {
	st := memstore.New()
	cache := newServiceCache(st)
	cache.cfg = cacheConfig{TTLs: defaultCacheTTLs(), Retention: time.Hour}
	require.NotNil(t, cache.noteSources, "memstore offers the capability")

	ctx := context.Background()
	require.NoError(t, st.RememberNoteSource(ctx, "acct-1", store.NoteSource{
		FeedID: "stale", Source: "pc_search", SeenAt: time.Now().Add(-2 * time.Hour),
	}))
	require.NoError(t, st.RememberNoteSource(ctx, "acct-1", store.NoteSource{
		FeedID: "fresh", Source: "pc_search", SeenAt: time.Now(),
	}))

	cache.prune()

	_, err := st.LookupNoteSource(ctx, "acct-1", "stale", 0)
	assert.ErrorIs(t, err, store.ErrNotFound, "a record past the provenance window is dead weight")
	_, err = st.LookupNoteSource(ctx, "acct-1", "fresh", 0)
	assert.NoError(t, err)
}

func TestAppendCommentsDeduplicatesOnRefetch(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")

	ctx := context.Background()
	tree := []xiaohongshu.Comment{
		{ID: "c1", NoteID: "note-1", SubComments: []xiaohongshu.Comment{{ID: "c1a"}}},
		{ID: "c2", NoteID: "note-1"},
	}
	assert.Equal(t, 3, svc.cache.appendComments(ctx, "note-1", tree))

	// The same note read again, with one new reply.
	tree[0].SubComments = append(tree[0].SubComments, xiaohongshu.Comment{ID: "c1b"})
	assert.Equal(t, 1, svc.cache.appendComments(ctx, "note-1", tree))

	rows, err := st.CommentsSince(ctx, "acct-1", "note-1", "", 0)
	require.NoError(t, err)
	require.Len(t, rows, 4)
}

// A cache hit read nothing from the site, so it has nothing to teach the log.
func TestCachedFeedDetailRecordsNoComments(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)

	ctx := context.Background()
	detail := &xiaohongshu.FeedDetailResponse{
		Note:     xiaohongshu.FeedDetail{NoteID: "note-1"},
		Comments: xiaohongshu.CommentList{List: []xiaohongshu.Comment{{ID: "c1", NoteID: "note-1"}}},
	}
	svc.cache.put(ctx, store.KindNote, "note-1", detail, nil, clock.Now())

	got, err := svc.GetFeedDetail(ctx, "note-1", "token", false)
	require.NoError(t, err)
	require.True(t, got.Cached)

	rows, err := st.CommentsSince(ctx, "acct-1", "note-1", "", 0)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
