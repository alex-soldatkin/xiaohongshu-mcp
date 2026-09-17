package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-rod/rod"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/pacing"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store/memstore"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// These tests exercise the read-through layer with no browser anywhere: the
// service's runHook stands in for the gate-and-page path, and the clock is
// injected so a TTL can expire without anything sleeping.

// testClock is a manually advanced clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// deleteCall is one recorded invalidation.
type deleteCall struct {
	kind store.Kind
	keys []string
}

// fakeStore counts every call made against it and records invalidations. When
// inner is set it delegates, so the same fake serves both "prove nothing was
// called" and "prove the right keys were dropped".
type fakeStore struct {
	mu      sync.Mutex
	calls   int
	deletes []deleteCall
	inner   store.Store
}

func (f *fakeStore) hit() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStore) deleteLog() []deleteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deleteCall(nil), f.deletes...)
}

func (f *fakeStore) GetDoc(ctx context.Context, account string, kind store.Kind, key string) (store.Doc, error) {
	f.hit()
	if f.inner != nil {
		return f.inner.GetDoc(ctx, account, kind, key)
	}
	return store.Doc{}, store.ErrNotFound
}

func (f *fakeStore) PutDoc(ctx context.Context, account string, kind store.Kind, key string, doc store.Doc) error {
	f.hit()
	if f.inner != nil {
		return f.inner.PutDoc(ctx, account, kind, key, doc)
	}
	return nil
}

func (f *fakeStore) DeleteDocs(ctx context.Context, account string, kind store.Kind, keys ...string) error {
	f.mu.Lock()
	f.calls++
	f.deletes = append(f.deletes, deleteCall{kind: kind, keys: append([]string(nil), keys...)})
	f.mu.Unlock()
	if f.inner != nil {
		return f.inner.DeleteDocs(ctx, account, kind, keys...)
	}
	return nil
}

func (f *fakeStore) AppendNotifications(ctx context.Context, account string, items []store.NotificationRecord) (int, error) {
	f.hit()
	return 0, nil
}

func (f *fakeStore) NotificationsSince(ctx context.Context, account, tab string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	f.hit()
	return nil, nil
}

func (f *fakeStore) AppendComments(ctx context.Context, account string, items []store.CommentRecord) (int, error) {
	f.hit()
	return 0, nil
}

func (f *fakeStore) CommentsSince(ctx context.Context, account, noteID string, after store.Cursor, limit int) ([]store.HistoryItem, error) {
	f.hit()
	return nil, nil
}

func (f *fakeStore) UpsertAccount(ctx context.Context, a store.Account) error {
	f.hit()
	if f.inner != nil {
		return f.inner.UpsertAccount(ctx, a)
	}
	return nil
}

func (f *fakeStore) AccountBySeed(ctx context.Context, seed int) (store.Account, error) {
	f.hit()
	if f.inner != nil {
		return f.inner.AccountBySeed(ctx, seed)
	}
	return store.Account{}, store.ErrNotFound
}

func (f *fakeStore) PruneDocs(ctx context.Context, olderThan time.Duration) (int64, error) {
	f.hit()
	return 0, nil
}

func (f *fakeStore) Close() error { return nil }

var _ store.Store = (*fakeStore)(nil)

// newCacheTestService builds a service whose only working part is the cache.
// The TTL table is reset to the built-in defaults so an ambient XHS_CACHE_*
// in the developer's shell cannot change what these tests mean.
func newCacheTestService(t *testing.T, st store.Store, clock *testClock) *XiaohongshuService {
	t.Helper()

	cache := newServiceCache(st)
	// Retention off: these tests age documents with a fake clock while the
	// store keeps its own, so a sweep would delete rows out from under them.
	// The sweep has its own tests below.
	cache.cfg = cacheConfig{TTLs: defaultCacheTTLs(), Retention: 0}
	cache.now = clock.Now
	cache.startRetention()
	t.Cleanup(cache.close)

	return &XiaohongshuService{cache: cache}
}

// errNoBrowser is what the stand-in browser path returns when a test expects
// the request to be served without one.
var errNoBrowser = errors.New("browser reached")

// failingRun asserts that the browser is never reached.
func failingRun(t *testing.T) func(context.Context, pacing.Class, func(*rod.Page) error) error {
	t.Helper()
	return func(context.Context, pacing.Class, func(*rod.Page) error) error {
		t.Errorf("the browser was reached when the cache should have served the request")
		return errNoBrowser
	}
}

// countingRun runs fn with a nil page and counts the calls.
func countingRun(calls *int) func(context.Context, pacing.Class, func(*rod.Page) error) error {
	return func(ctx context.Context, class pacing.Class, fn func(*rod.Page) error) error {
		*calls++
		return fn(nil)
	}
}

type cacheTestPayload struct {
	Value string `json:"value"`
}

func TestReadThroughServesAHitWithinTTL(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")

	fetches := 0
	svc.runHook = countingRun(&fetches)

	fetch := func(*rod.Page) (*cacheTestPayload, error) {
		return &cacheTestPayload{Value: "live"}, nil
	}

	ctx := context.Background()
	first, firstAt, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.False(t, cached)
	assert.Equal(t, "live", first.Value)
	assert.Equal(t, 1, fetches)

	clock.Advance(30 * time.Minute)

	second, secondAt, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.True(t, cached, "a document inside its TTL must be served from the store")
	assert.Equal(t, "live", second.Value)
	assert.Equal(t, 1, fetches, "a hit must not reach the browser at all")
	assert.True(t, firstAt.Equal(secondAt), "a hit reports when the payload was fetched, not when it was served")
}

func TestReadThroughMissesPastTTL(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")

	fetches := 0
	svc.runHook = countingRun(&fetches)

	fetch := func(*rod.Page) (*cacheTestPayload, error) {
		return &cacheTestPayload{Value: "live"}, nil
	}

	ctx := context.Background()
	_, _, _, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)

	clock.Advance(time.Hour + time.Second)

	_, _, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.False(t, cached)
	assert.Equal(t, 2, fetches, "expiry triggers exactly one refetch")
}

func TestReadThroughZeroTTLDisablesTheKind(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")
	svc.cache.cfg.TTLs[store.KindFeed] = 0

	fetches := 0
	svc.runHook = countingRun(&fetches)
	fetch := func(*rod.Page) (*cacheTestPayload, error) { return &cacheTestPayload{Value: "live"}, nil }

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, _, cached, err := readThrough(ctx, svc, store.KindFeed, "home", svc.cache.ttl(store.KindFeed), nil, nil, fetch)
		require.NoError(t, err)
		assert.False(t, cached)
	}
	assert.Equal(t, 2, fetches)

	_, err := st.GetDoc(ctx, "acct-1", store.KindFeed, "home")
	assert.ErrorIs(t, err, store.ErrNotFound, "a disabled kind must not be written either")
}

func TestReadThroughForceRefreshBypassesAndOverwrites(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")

	fetches := 0
	svc.runHook = countingRun(&fetches)

	value := "first"
	fetch := func(*rod.Page) (*cacheTestPayload, error) { return &cacheTestPayload{Value: value}, nil }

	ctx := context.Background()
	_, _, _, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)

	value = "second"
	forced := withForceRefresh(ctx, true)
	got, _, cached, err := readThrough(forced, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.False(t, cached)
	assert.Equal(t, "second", got.Value)
	assert.Equal(t, 2, fetches)

	// The forced fetch must have replaced the stored document, not left the
	// stale one behind for the next reader.
	after, _, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.True(t, cached)
	assert.Equal(t, "second", after.Value)
	assert.Equal(t, 2, fetches)
}

func TestReadThroughUnknownAccountMissesThenPuts(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	require.Empty(t, svc.cache.accountID(), "a fresh database knows no account")

	fetches := 0
	svc.runHook = countingRun(&fetches)

	// Stand in for observeAccount: the real one reads the user id out of the
	// page that just served the fetch.
	fetch := func(*rod.Page) (*cacheTestPayload, error) {
		svc.cache.rememberAccount(context.Background(), "acct-9", "nine")
		return &cacheTestPayload{Value: "live"}, nil
	}

	ctx := context.Background()
	_, _, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.False(t, cached)
	assert.Equal(t, "acct-9", svc.cache.accountID())

	// The document was written under the account learned during that same
	// fetch, so the very next read hits.
	_, _, cached, err = readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
	require.NoError(t, err)
	assert.True(t, cached)
	assert.Equal(t, 1, fetches)
}

// TestNopStoreTouchesNothing is the byte-identical-behaviour guarantee: with
// no database configured, the cache layer must call no store method at all and
// must fetch every time.
//
// The fake is installed behind a disabled cache rather than passed to
// newServiceCache, because a fake is not a store.Nop and would be taken for a
// real backend. That the unset deployment produces a disabled cache is
// asserted separately, just above.
func TestNopStoreTouchesNothing(t *testing.T) {
	assert.False(t, newServiceCache(store.Nop{}).enabled, "an unset XHS_DATABASE_URL disables the cache")
	assert.False(t, newServiceCache(nil).enabled, "no store at all disables the cache")

	clock := newTestClock()
	fake := &fakeStore{}
	svc := newCacheTestService(t, store.Nop{}, clock)
	svc.cache.store = fake
	svc.cache.setAccount("acct-1")

	fetches := 0
	svc.runHook = countingRun(&fetches)
	fetch := func(*rod.Page) (*cacheTestPayload, error) { return &cacheTestPayload{Value: "live"}, nil }

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, fetchedAt, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
		require.NoError(t, err)
		assert.False(t, cached)
		assert.Equal(t, "live", got.Value)
		assert.True(t, fetchedAt.IsZero(), "no store means no freshness metadata to report")
	}

	// Invalidation and account bookkeeping must be just as silent.
	svc.cache.invalidateNote(ctx, "note-1")
	svc.cache.invalidate(ctx, store.KindMyProfile)
	svc.cache.rememberAccount(ctx, "acct-2", "two")
	svc.cache.seedAccount(ctx)

	assert.Equal(t, 3, fetches, "every read goes to the browser")
	assert.Equal(t, 0, fake.callCount(), "no store method may be called")
	assert.Empty(t, fake.deleteLog())
}

func TestCacheMetaOmittedWithoutStore(t *testing.T) {
	// The unset deployment's JSON must be what it was before WS2.
	raw, err := json.Marshal(&FeedsListResponse{Feeds: []xiaohongshu.Feed{}, Count: 0})
	require.NoError(t, err)
	assert.JSONEq(t, `{"feeds":[],"count":0}`, string(raw))

	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	raw, err = json.Marshal(&FeedsListResponse{Count: 0, CacheMeta: newCacheMeta(at, true)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"feeds":null,"count":0,"cached":true,"fetched_at":"2026-03-01T12:00:00Z"}`, string(raw))

	// A live fetch against a configured store reports when it happened but
	// does not claim to be cached.
	raw, err = json.Marshal(&FeedsListResponse{Count: 0, CacheMeta: newCacheMeta(at, false)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"feeds":null,"count":0,"fetched_at":"2026-03-01T12:00:00Z"}`, string(raw))
}

// ---------------------------------------------------------------------------
// per-method wiring
// ---------------------------------------------------------------------------

func TestGetFeedDetailIsServedByANoteFullDocument(t *testing.T) {
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)

	detail := &xiaohongshu.FeedDetailResponse{
		Note: xiaohongshu.FeedDetail{NoteID: "note-1", Title: "标题"},
		Comments: xiaohongshu.CommentList{
			List: []xiaohongshu.Comment{{ID: "c1", Content: "hi"}},
		},
	}
	ctx := context.Background()
	svc.cache.put(ctx, store.KindNoteFull, "note-1", detail,
		newCommentConfigMeta(xiaohongshu.CommentLoadConfig{MaxCommentItems: 50, MaxRepliesThreshold: 10}),
		clock.Now())

	// A plain request (load_all_comments=false) is satisfied by the fuller
	// document; that is the whole point of looking note_full up first.
	got, err := svc.GetFeedDetail(ctx, "note-1", "token", false)
	require.NoError(t, err)
	assert.True(t, got.Cached)
	assert.Equal(t, "note-1", got.FeedID)

	data, ok := got.Data.(*xiaohongshu.FeedDetailResponse)
	require.True(t, ok)
	assert.Equal(t, "标题", data.Note.Title)
	require.Len(t, data.Comments.List, 1)
}

func TestGetFeedDetailRejectsALessCompleteNoteFull(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")

	fetches := 0
	svc.runHook = func(ctx context.Context, class pacing.Class, fn func(*rod.Page) error) error {
		fetches++
		return errNoBrowser
	}

	ctx := context.Background()
	svc.cache.put(ctx, store.KindNoteFull, "note-1", &xiaohongshu.FeedDetailResponse{},
		newCommentConfigMeta(xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 10}),
		clock.Now())

	// 100 comments were asked for and only 20 were ever loaded.
	_, err := svc.GetFeedDetailWithConfig(ctx, "note-1", "token", true,
		xiaohongshu.CommentLoadConfig{MaxCommentItems: 100, MaxRepliesThreshold: 10})
	require.ErrorIs(t, err, errNoBrowser)
	assert.Equal(t, 1, fetches)

	// A request for 20 is served from the same document.
	svc.runHook = failingRun(t)
	got, err := svc.GetFeedDetailWithConfig(ctx, "note-1", "token", true,
		xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 10})
	require.NoError(t, err)
	assert.True(t, got.Cached)
}

func TestGetFeedDetailLoadAllIsNotServedByAPlainNote(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")

	fetches := 0
	svc.runHook = func(ctx context.Context, class pacing.Class, fn func(*rod.Page) error) error {
		fetches++
		return errNoBrowser
	}

	ctx := context.Background()
	svc.cache.put(ctx, store.KindNote, "note-1", &xiaohongshu.FeedDetailResponse{}, nil, clock.Now())

	_, err := svc.GetFeedDetailWithConfig(ctx, "note-1", "token", true, xiaohongshu.DefaultCommentLoadConfig())
	require.ErrorIs(t, err, errNoBrowser)
	assert.Equal(t, 1, fetches, "a note holds only the first page of comments")
}

func TestAcceptCommentConfig(t *testing.T) {
	want := xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 10, ClickMoreReplies: true}
	accept := acceptCommentConfig(want)

	metaOf := func(c xiaohongshu.CommentLoadConfig) json.RawMessage {
		raw, err := json.Marshal(newCommentConfigMeta(c))
		require.NoError(t, err)
		return raw
	}

	assert.True(t, accept(metaOf(want)), "the identical config accepts itself")
	assert.True(t, accept(metaOf(xiaohongshu.CommentLoadConfig{MaxCommentItems: 50, MaxRepliesThreshold: 10, ClickMoreReplies: true})),
		"a more complete document serves a smaller request")
	assert.False(t, accept(metaOf(xiaohongshu.CommentLoadConfig{MaxCommentItems: 10, MaxRepliesThreshold: 10, ClickMoreReplies: true})),
		"fewer comments than asked for")
	assert.False(t, accept(metaOf(xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 10})),
		"sub-replies were asked for and were never expanded")
	assert.False(t, accept(metaOf(xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 3, ClickMoreReplies: true})),
		"a lower threshold skipped comments this request would have kept")
	assert.False(t, accept(nil), "a document with no meta predates the rule and cannot be judged")
	assert.False(t, accept(json.RawMessage(`not json`)))

	// Without sub-replies the threshold is irrelevant.
	plain := acceptCommentConfig(xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 10})
	assert.True(t, plain(metaOf(xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 1})))
}

func TestAcceptNotificationLimit(t *testing.T) {
	metaOf := func(limit int) json.RawMessage {
		raw, err := json.Marshal(notificationListMeta{Limit: limit})
		require.NoError(t, err)
		return raw
	}

	assert.True(t, acceptNotificationLimit(20)(metaOf(20)))
	assert.True(t, acceptNotificationLimit(20)(metaOf(50)), "a longer listing serves a shorter request")
	assert.False(t, acceptNotificationLimit(50)(metaOf(20)), "a shorter listing cannot serve a longer request")
	assert.True(t, acceptNotificationLimit(20)(metaOf(0)), "an unlimited listing serves anything")
	assert.False(t, acceptNotificationLimit(0)(metaOf(20)), "only an unlimited listing serves an unlimited request")
	assert.False(t, acceptNotificationLimit(20)(nil))
}

func TestListNotificationsTruncatesALongerCachedListing(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)

	ctx := context.Background()
	list := &xiaohongshu.NotificationList{Tab: xiaohongshu.TabMentions, Filtered: 1}
	for i := 0; i < 5; i++ {
		list.Items = append(list.Items, xiaohongshu.NotificationItem{ID: string(rune('a' + i))})
	}
	svc.cache.put(ctx, store.KindNotifications, string(xiaohongshu.TabMentions), list,
		notificationListMeta{Limit: 5}, clock.Now())

	got, err := svc.ListNotifications(ctx, "mentions", 2, "")
	require.NoError(t, err)
	assert.True(t, got.Cached)
	require.Len(t, got.Items, 2)
	assert.Equal(t, "a", got.Items[0].ID)
}

func TestReadMethodsAreServedFromTheirOwnKind(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)
	ctx := context.Background()

	profile := &UserProfileResponse{UserBasicInfo: xiaohongshu.UserBasicInfo{Nickname: "someone"}}
	svc.cache.put(ctx, store.KindProfile, profileCacheKey("user-1", xiaohongshu.TabNotes), profile, nil, clock.Now())
	gotProfile, err := svc.UserProfile(ctx, "user-1", "token", "note")
	require.NoError(t, err)
	assert.True(t, gotProfile.Cached)
	assert.Equal(t, "someone", gotProfile.UserBasicInfo.Nickname)

	mine := &UserProfileResponse{UserBasicInfo: xiaohongshu.UserBasicInfo{Nickname: "me"}}
	svc.cache.put(ctx, store.KindMyProfile, string(xiaohongshu.TabFavorites), mine, nil, clock.Now())
	gotMine, err := svc.GetMyProfile(ctx, "fav")
	require.NoError(t, err)
	assert.True(t, gotMine.Cached)
	assert.Equal(t, "me", gotMine.UserBasicInfo.Nickname)

	feeds := &FeedsListResponse{Feeds: []xiaohongshu.Feed{{ID: "f1"}}, Count: 1}
	svc.cache.put(ctx, store.KindFeed, "home", feeds, nil, clock.Now())
	gotFeeds, err := svc.ListFeeds(ctx)
	require.NoError(t, err)
	assert.True(t, gotFeeds.Cached)
	assert.Equal(t, 1, gotFeeds.Count)

	filter := xiaohongshu.FilterOption{SortBy: "最新"}
	svc.cache.put(ctx, store.KindSearch, searchCacheKey("露营", filter), feeds, nil, clock.Now())
	gotSearch, err := svc.SearchFeeds(ctx, "露营", filter)
	require.NoError(t, err)
	assert.True(t, gotSearch.Cached)

	unread := &xiaohongshu.NotificationCount{Mentions: 3, Unread: 3}
	svc.cache.put(ctx, store.KindUnread, "count", unread, nil, clock.Now())
	gotUnread, err := svc.GetUnreadCount(ctx)
	require.NoError(t, err)
	assert.True(t, gotUnread.Cached)
	assert.Equal(t, 3, gotUnread.Mentions)
}

func TestSearchCacheKeyDistinguishesFilters(t *testing.T) {
	base := searchCacheKey("露营", xiaohongshu.FilterOption{})
	assert.Len(t, base, 16)
	assert.Equal(t, base, searchCacheKey("露营", xiaohongshu.FilterOption{}), "the key is stable")
	assert.NotEqual(t, base, searchCacheKey("露营", xiaohongshu.FilterOption{SortBy: "最新"}))
	assert.NotEqual(t, base, searchCacheKey("野餐", xiaohongshu.FilterOption{}))
}

// ---------------------------------------------------------------------------
// invalidation
// ---------------------------------------------------------------------------

// TestWriteMethodsInvalidateBeforeTheAction pins the plan's invalidation table.
// Every case runs with a browser path that fails, which also proves the
// invalidation happens before the action rather than after it: an action that
// never succeeded still dropped the documents it could have changed.
func TestWriteMethodsInvalidateBeforeTheAction(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, svc *XiaohongshuService) error
		want []deleteCall
	}{
		{
			name: "like",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.LikeFeed(ctx, "note-1", "token")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNote, keys: []string{"note-1"}},
				{kind: store.KindNoteFull, keys: []string{"note-1"}},
				{kind: store.KindMyProfile, keys: []string{"liked"}},
			},
		},
		{
			name: "unlike",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.UnlikeFeed(ctx, "note-1", "token")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNote, keys: []string{"note-1"}},
				{kind: store.KindNoteFull, keys: []string{"note-1"}},
				{kind: store.KindMyProfile, keys: []string{"liked"}},
			},
		},
		{
			name: "favorite",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.FavoriteFeed(ctx, "note-1", "token")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNote, keys: []string{"note-1"}},
				{kind: store.KindNoteFull, keys: []string{"note-1"}},
				{kind: store.KindMyProfile, keys: []string{"fav"}},
			},
		},
		{
			name: "unfavorite",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.UnfavoriteFeed(ctx, "note-1", "token")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNote, keys: []string{"note-1"}},
				{kind: store.KindNoteFull, keys: []string{"note-1"}},
				{kind: store.KindMyProfile, keys: []string{"fav"}},
			},
		},
		{
			name: "comment",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.PostCommentToFeed(ctx, "note-1", "token", "hi")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNote, keys: []string{"note-1"}},
				{kind: store.KindNoteFull, keys: []string{"note-1"}},
			},
		},
		{
			name: "reply comment",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.ReplyCommentToFeed(ctx, "note-1", "token", "c1", "u1", "hi")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNote, keys: []string{"note-1"}},
				{kind: store.KindNoteFull, keys: []string{"note-1"}},
			},
		},
		{
			name: "publish content",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				return svc.publishContent(ctx, xiaohongshu.PublishImageContent{Title: "t"})
			},
			// No keys means every tab of the account's own profile.
			want: []deleteCall{{kind: store.KindMyProfile}},
		},
		{
			name: "publish video",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				return svc.publishVideo(ctx, xiaohongshu.PublishVideoContent{Title: "t"})
			},
			want: []deleteCall{{kind: store.KindMyProfile}},
		},
		{
			name: "like notification",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.LikeNotification(ctx, "c1", false)
				return err
			},
			want: []deleteCall{
				{kind: store.KindNotifications},
				{kind: store.KindUnread},
			},
		},
		{
			name: "reply notification",
			call: func(ctx context.Context, svc *XiaohongshuService) error {
				_, err := svc.ReplyNotification(ctx, "c1", "hi")
				return err
			},
			want: []deleteCall{
				{kind: store.KindNotifications},
				{kind: store.KindUnread},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newTestClock()
			fake := &fakeStore{inner: memstore.New()}
			svc := newCacheTestService(t, fake, clock)
			svc.cache.setAccount("acct-1")
			svc.runHook = func(context.Context, pacing.Class, func(*rod.Page) error) error {
				return errNoBrowser
			}

			err := tc.call(context.Background(), svc)
			require.ErrorIs(t, err, errNoBrowser)

			got := fake.deleteLog()
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				assert.Equal(t, tc.want[i].kind, got[i].kind)
				assert.Equal(t, tc.want[i].keys, got[i].keys)
			}
		})
	}
}

// TestListNotificationsHitLeavesTheUnreadCountAlone is the other half of the
// side-effect rule. A live listing clears the tab's unread marks on the site,
// so the method drops the cached unread count afterwards. A cached listing
// touches nothing, so it must leave that count exactly where it is — dropping
// it there would force a needless browser trip for a number that is still
// correct.
func TestListNotificationsHitLeavesTheUnreadCountAlone(t *testing.T) {
	clock := newTestClock()
	fake := &fakeStore{inner: memstore.New()}
	svc := newCacheTestService(t, fake, clock)
	svc.cache.setAccount("acct-1")

	ctx := context.Background()
	svc.cache.put(ctx, store.KindNotifications, string(xiaohongshu.TabMentions),
		&xiaohongshu.NotificationList{Tab: xiaohongshu.TabMentions},
		notificationListMeta{Limit: 20}, clock.Now())

	svc.runHook = failingRun(t)
	got, err := svc.ListNotifications(ctx, "mentions", 20, "")
	require.NoError(t, err)
	assert.True(t, got.Cached)
	assert.Empty(t, fake.deleteLog(), "a cached listing changed nothing on the site")
}

func TestDeleteCookiesForgetsTheAccount(t *testing.T) {
	clock := newTestClock()
	svc := newCacheTestService(t, memstore.New(), clock)
	svc.cache.setAccount("acct-1")

	svc.cache.clearAccount()
	assert.Empty(t, svc.cache.accountID())

	// With no account, lookups miss and nothing is written under a guess.
	fetches := 0
	svc.runHook = countingRun(&fetches)
	fetch := func(*rod.Page) (*cacheTestPayload, error) { return &cacheTestPayload{Value: "live"}, nil }

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, _, cached, err := readThrough(ctx, svc, store.KindFeed, "home", time.Hour, nil, nil, fetch)
		require.NoError(t, err)
		assert.False(t, cached)
	}
	assert.Equal(t, 2, fetches)
}

// ---------------------------------------------------------------------------
// round-trip fidelity
// ---------------------------------------------------------------------------

// TestFeedDetailSurvivesTheCacheRoundTrip is the guard against a cached
// response differing from a live one. VideoDetail has a custom UnmarshalJSON
// that lifts subtitles out of a stringified mediaV2 copy; if the marshalled
// form ever stops carrying everything the unmarshalled form needs, a cached
// video note would come back subtly poorer than a freshly fetched one.
func TestFeedDetailSurvivesTheCacheRoundTrip(t *testing.T) {
	original := &xiaohongshu.FeedDetailResponse{
		Note: xiaohongshu.FeedDetail{
			NoteID:     "note-1",
			XsecToken:  "token",
			Title:      "露营的一天",
			Desc:       "带 emoji 的正文 🏕",
			Type:       "video",
			Time:       1740000000000,
			IPLocation: "上海",
			User:       xiaohongshu.User{UserID: "u1", Nickname: "作者"},
			InteractInfo: xiaohongshu.InteractInfo{
				Liked:        true,
				LikedCount:   "1024",
				Collected:    true,
				CommentCount: "12",
			},
			ImageList: []xiaohongshu.DetailImageInfo{
				{Width: 1080, Height: 1440, URLDefault: "https://example.com/a.jpg", LivePhoto: true},
			},
			Video: &xiaohongshu.VideoDetail{
				Image: xiaohongshu.VideoImage{FirstFrameFileID: "ff", ThumbnailFileID: "tn"},
				Capa:  xiaohongshu.VideoCapability{Duration: 42},
				Media: xiaohongshu.VideoMedia{
					VideoID: 987654321,
					Video:   xiaohongshu.VideoMeta{Duration: 42, MD5: "abc", StreamTypes: []int{1, 2}},
					Stream: map[string][]xiaohongshu.VideoStream{
						"h264": {{
							MasterURL:  "https://example.com/v.mp4?sign=x",
							BackupURLs: []string{"https://example.com/v-backup.mp4"},
							Format:     "mp4", Width: 1080, Height: 1920, Duration: 42000,
							Size: 12345678, FPS: 30, VideoCodec: "h264", AudioCodec: "aac",
							Volume: 0.8, VMAF: 93.5,
						}},
						"h265": {},
					},
				},
				Subtitles: map[string][]xiaohongshu.VideoSubtitle{
					"zh":     {{URL: "https://example.com/zh.srt", Language: "zh", Format: 1, Type: 2}},
					"source": {{URL: "https://example.com/src.srt", Language: "zh"}},
				},
			},
		},
		Comments: xiaohongshu.CommentList{
			Cursor:  "cursor-1",
			HasMore: true,
			List: []xiaohongshu.Comment{
				{
					ID: "c1", NoteID: "note-1", Content: "顶", LikeCount: "3",
					CreateTime: 1740000001000, IPLocation: "北京", Liked: true,
					UserInfo:        xiaohongshu.User{UserID: "u2", Nickname: "读者"},
					SubCommentCount: "2",
					ShowTags:        []string{"作者"},
					SubComments: []xiaohongshu.Comment{
						{
							ID: "c1-1", NoteID: "note-1", Content: "同意",
							UserInfo:        xiaohongshu.User{UserID: "u3"},
							SubCommentCount: "1",
							SubComments: []xiaohongshu.Comment{
								{ID: "c1-1-1", NoteID: "note-1", Content: "第三层"},
							},
						},
						{ID: "c1-2", NoteID: "note-1", Content: "再来一条"},
					},
				},
			},
		},
	}

	raw, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded xiaohongshu.FeedDetailResponse
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, *original, decoded)

	// And once more through the store, which is how it really travels. The
	// payload is compared as JSON, never as bytes: WS1 established that a
	// database column may normalise the encoding.
	clock := newTestClock()
	st := memstore.New()
	svc := newCacheTestService(t, st, clock)
	svc.cache.setAccount("acct-1")
	svc.runHook = failingRun(t)

	ctx := context.Background()
	svc.cache.put(ctx, store.KindNoteFull, "note-1", original,
		newCommentConfigMeta(xiaohongshu.CommentLoadConfig{MaxCommentItems: 20, MaxRepliesThreshold: 10}), clock.Now())

	got, err := svc.GetFeedDetail(ctx, "note-1", "token", false)
	require.NoError(t, err)
	fromCache, ok := got.Data.(*xiaohongshu.FeedDetailResponse)
	require.True(t, ok)
	require.Equal(t, original, fromCache)

	roundTripped, err := json.Marshal(fromCache)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(roundTripped))
}

// ---------------------------------------------------------------------------
// configuration, retention and force_refresh plumbing
// ---------------------------------------------------------------------------

func TestCacheConfigFromEnv(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := cacheConfigFromEnv()
		assert.Equal(t, defaultTTLNote, cfg.TTLs[store.KindNote])
		assert.Equal(t, defaultTTLNote, cfg.TTLs[store.KindNoteFull])
		assert.Equal(t, defaultTTLUnread, cfg.TTLs[store.KindUnread])
		assert.Equal(t, defaultCacheRetention, cfg.Retention)
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("XHS_CACHE_TTL_NOTE", "90m")
		t.Setenv("XHS_CACHE_TTL_FEED", "0")
		t.Setenv("XHS_CACHE_TTL_UNREAD", "30")
		t.Setenv("XHS_CACHE_RETENTION", "24h")

		cfg := cacheConfigFromEnv()
		assert.Equal(t, 90*time.Minute, cfg.TTLs[store.KindNote])
		assert.Equal(t, 90*time.Minute, cfg.TTLs[store.KindNoteFull], "note and note_full share one knob")
		assert.Zero(t, cfg.TTLs[store.KindFeed], "zero disables the kind")
		assert.Equal(t, 30*time.Second, cfg.TTLs[store.KindUnread], "a bare number is seconds")
		assert.Equal(t, 24*time.Hour, cfg.Retention)
	})

	t.Run("garbage is ignored", func(t *testing.T) {
		t.Setenv("XHS_CACHE_TTL_SEARCH", "soon")
		t.Setenv("XHS_CACHE_RETENTION", "-1")
		cfg := cacheConfigFromEnv()
		assert.Equal(t, defaultTTLSearch, cfg.TTLs[store.KindSearch])
		assert.Equal(t, defaultCacheRetention, cfg.Retention)
	})
}

func TestRetentionSweepRunsAtStartupAndStops(t *testing.T) {
	fake := &fakeStore{inner: memstore.New()}
	cache := newServiceCache(fake)
	cache.cfg.Retention = time.Hour
	cache.startRetention()

	// PruneDocs is called once at startup; the hourly tick is not waited for.
	deadline := time.Now().Add(2 * time.Second)
	for fake.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	assert.GreaterOrEqual(t, fake.callCount(), 1)

	cache.close()
	cache.close() // idempotent
}

func TestRetentionDisabledByZero(t *testing.T) {
	fake := &fakeStore{}
	cache := newServiceCache(fake)
	cache.cfg.Retention = 0
	cache.startRetention()
	cache.close()
	assert.Zero(t, fake.callCount(), "a zero retention window must not prune anything")
}

func TestForceRefreshContext(t *testing.T) {
	ctx := context.Background()
	assert.False(t, forceRefresh(ctx))
	assert.True(t, withForceRefresh(ctx, false) == ctx, "the common case allocates nothing")
	assert.True(t, forceRefresh(withForceRefresh(ctx, true)))
}

func TestForceRefreshMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name   string
		url    string
		header string
		want   bool
	}{
		{name: "absent", url: "/x", want: false},
		{name: "query 1", url: "/x?force_refresh=1", want: true},
		{name: "query true", url: "/x?force_refresh=true", want: true},
		{name: "query 0", url: "/x?force_refresh=0", want: false},
		{name: "query nonsense", url: "/x?force_refresh=maybe", want: false},
		{name: "header", url: "/x", header: "1", want: true},
		{name: "header yes", url: "/x", header: "YES", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen bool
			router := gin.New()
			router.Use(forceRefreshMiddleware())
			router.GET("/x", func(c *gin.Context) {
				seen = forceRefresh(c.Request.Context())
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			if tc.header != "" {
				req.Header.Set("X-Force-Refresh", tc.header)
			}
			router.ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tc.want, seen)
		})
	}
}

// ---------------------------------------------------------------------------
// xsec_token lifetime clamp (issue #18, workstream F)
// ---------------------------------------------------------------------------

// A listing is not just text: every note in it carries a token the agent then
// navigates with. Serving one past the token's measured lifetime hands out
// arguments that fail, so the listing TTLs are capped at it. Note details are
// not, because nothing navigates with FeedDetail.XsecToken.
func TestTokenLifetimeClampsListingTTLs(t *testing.T) {
	cfg := cacheConfig{TTLs: defaultCacheTTLs()}
	require.Equal(t, defaultTTLProfile, cfg.TTLs[store.KindProfile])

	cfg.clampToTokenLifetime(time.Hour)

	assert.Equal(t, time.Hour, cfg.TTLs[store.KindProfile],
		"the 6h profile TTL is the one the measurement in #18 does not cover")
	assert.Equal(t, defaultTTLMyProfile, cfg.TTLs[store.KindMyProfile], "already inside the window")
	assert.Equal(t, defaultTTLFeed, cfg.TTLs[store.KindFeed])
	assert.Equal(t, defaultTTLSearch, cfg.TTLs[store.KindSearch])
	assert.Equal(t, defaultTTLNotifications, cfg.TTLs[store.KindNotifications])
	assert.Equal(t, defaultTTLNote, cfg.TTLs[store.KindNote], "note details are never clamped")
	assert.Equal(t, defaultTTLNote, cfg.TTLs[store.KindNoteFull])
}

// Zero is the "unknown lifetime" setting the knob shipped with before the
// measurement existed, and it must clamp nothing at all.
func TestTokenLifetimeZeroClampsNothing(t *testing.T) {
	cfg := cacheConfig{TTLs: defaultCacheTTLs()}
	cfg.clampToTokenLifetime(0)
	assert.Equal(t, defaultTTLProfile, cfg.TTLs[store.KindProfile])
}

func TestTokenLifetimeIsConfigurable(t *testing.T) {
	t.Setenv("XHS_XSEC_TOKEN_LIFETIME", "10m")
	cfg := cacheConfigFromEnv()
	assert.Equal(t, 10*time.Minute, cfg.TTLs[store.KindProfile])
	assert.Equal(t, 5*time.Minute, cfg.TTLs[store.KindFeed], "a shorter TTL is left where it is")

	t.Setenv("XHS_XSEC_TOKEN_LIFETIME", "0")
	assert.Equal(t, defaultTTLProfile, cacheConfigFromEnv().TTLs[store.KindProfile])

	t.Setenv("XHS_XSEC_TOKEN_LIFETIME", "")
	assert.Equal(t, xsecTokenLifetime, cacheConfigFromEnv().TTLs[store.KindProfile],
		"unset means the measured default, not no clamp")
}
