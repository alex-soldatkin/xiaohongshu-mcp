// Package storetest is the executable specification of store.Store.
//
// Any backend — the in-memory one, the Postgres one, a future Mongo one — is
// correct exactly insofar as it passes storetest.Run. Where this suite and the
// prose in store.go disagree, this suite wins, because it is the thing a new
// backend is actually checked against.
//
// Two things are deliberately *not* asserted, because demanding them would
// rule out a reasonable backend:
//
//   - Payloads compare as JSON, never as bytes. A JSONB column reorders object
//     keys and drops insignificant whitespace, so byte-identical round tripping
//     is not achievable in Postgres and must not be relied on. Callers that
//     need a stable response body get it by unmarshalling into a typed struct
//     and re-marshalling, not from the store.
//   - Cursor values are never interpreted. The suite only requires that they
//     are unique, stable, and ordered; it never assumes they are numbers.
package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
)

// Run executes the full contract suite against a backend.
//
// newStore must return a store that is empty and isolated: subtests run
// independently and several of them assume nothing else has written to the
// account ids they use. For a database backend that means a fresh schema, a
// fresh database or a per-test account-id prefix — Run itself makes no attempt
// to clean up, and calls Close on whatever it is given.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()

	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"Docs/PutGet", testDocsPutGet},
		{"Docs/Overwrite", testDocsOverwrite},
		{"Docs/NotFound", testDocsNotFound},
		{"Docs/Isolation", testDocsIsolation},
		{"Docs/DeleteByKey", testDocsDeleteByKey},
		{"Docs/DeleteWholeKind", testDocsDeleteWholeKind},
		{"Docs/EmptyMeta", testDocsEmptyMeta},
		{"Notifications/AppendDedup", testNotificationsAppendDedup},
		{"Notifications/TabIsPartOfIdentity", testNotificationsTabIdentity},
		{"Notifications/Pagination", testNotificationsPagination},
		{"Notifications/CursorsAreStable", testNotificationsCursorsStable},
		{"Notifications/Scoping", testNotificationsScoping},
		{"Comments/AppendAndRead", testCommentsAppendAndRead},
		{"Accounts/Upsert", testAccountsUpsert},
		{"Accounts/BySeed", testAccountsBySeed},
		{"Prune/ByAge", testPruneByAge},
		{"Prune/NonPositiveIsNoop", testPruneNonPositiveIsNoop},
		{"Prune/LeavesHistory", testPruneLeavesHistory},
		{"Close/Idempotent", testCloseIdempotent},
		{"Concurrency/MixedTraffic", testConcurrency},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			tc.fn(t, s)
		})
	}
}

const (
	acctA = "acct-A"
	acctB = "acct-B"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func raw(format string, args ...any) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(format, args...))
}

// requireDoc asserts a document round tripped: payload and meta equal as JSON,
// and FetchedAt preserves the instant. Backends store timestamps at
// microsecond resolution, so the comparison allows a microsecond of slack and
// compares instants rather than wall-clock representations.
func requireDoc(t *testing.T, want store.Doc, got store.Doc) {
	t.Helper()
	require.JSONEq(t, string(want.Payload), string(got.Payload))
	require.WithinDuration(t, want.FetchedAt, got.FetchedAt, time.Microsecond)
}

func testDocsPutGet(t *testing.T, s store.Store) {
	c := ctx(t)
	fetched := time.Now().Add(-90 * time.Minute)
	want := store.Doc{
		Payload:   raw(`{"id":"n1","title":"hello","likes":3}`),
		Meta:      raw(`{"max_comments":20}`),
		FetchedAt: fetched,
	}

	require.NoError(t, s.PutDoc(c, acctA, store.KindNote, "n1", want))

	got, err := s.GetDoc(c, acctA, store.KindNote, "n1")
	require.NoError(t, err)
	requireDoc(t, want, got)
	require.JSONEq(t, string(want.Meta), string(got.Meta))

	// The store never judges freshness: a document fetched long ago comes back
	// exactly like a fresh one, and TTL is the caller's decision.
	require.False(t, got.FetchedAt.IsZero())
}

func testDocsOverwrite(t *testing.T, s store.Store) {
	c := ctx(t)
	first := time.Now().Add(-2 * time.Hour)
	second := time.Now()

	require.NoError(t, s.PutDoc(c, acctA, store.KindNote, "n1", store.Doc{
		Payload: raw(`{"v":1}`), Meta: raw(`{"m":1}`), FetchedAt: first,
	}))
	require.NoError(t, s.PutDoc(c, acctA, store.KindNote, "n1", store.Doc{
		Payload: raw(`{"v":2}`), Meta: raw(`{"m":2}`), FetchedAt: second,
	}))

	got, err := s.GetDoc(c, acctA, store.KindNote, "n1")
	require.NoError(t, err)
	require.JSONEq(t, `{"v":2}`, string(got.Payload))
	require.JSONEq(t, `{"m":2}`, string(got.Meta))
	require.WithinDuration(t, second, got.FetchedAt, time.Microsecond)
}

func testDocsNotFound(t *testing.T, s store.Store) {
	c := ctx(t)

	got, err := s.GetDoc(c, acctA, store.KindNote, "absent")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Equal(t, store.Doc{}, got, "a miss must not return a half-filled Doc")

	// Deleting something that is not there is not an error. The cache layer
	// invalidates optimistically before every write and must not have to know
	// whether a document was ever cached.
	require.NoError(t, s.DeleteDocs(c, acctA, store.KindNote, "absent"))
	require.NoError(t, s.DeleteDocs(c, acctA, store.KindNote))
}

func testDocsIsolation(t *testing.T, s store.Store) {
	c := ctx(t)
	put := func(account string, kind store.Kind, key, v string) {
		require.NoError(t, s.PutDoc(c, account, kind, key, store.Doc{
			Payload: raw(`{"v":%q}`, v), FetchedAt: time.Now(),
		}))
	}

	// The same key under a different account or a different kind is a
	// different document. Cross-account sharing is explicitly not a feature:
	// a note cached for A carries A's "liked" flags.
	put(acctA, store.KindNote, "k", "a-note")
	put(acctB, store.KindNote, "k", "b-note")
	put(acctA, store.KindNoteFull, "k", "a-note-full")

	for _, tc := range []struct {
		account string
		kind    store.Kind
		want    string
	}{
		{acctA, store.KindNote, "a-note"},
		{acctB, store.KindNote, "b-note"},
		{acctA, store.KindNoteFull, "a-note-full"},
	} {
		got, err := s.GetDoc(c, tc.account, tc.kind, "k")
		require.NoError(t, err)
		require.JSONEq(t, fmt.Sprintf(`{"v":%q}`, tc.want), string(got.Payload))
	}

	_, err := s.GetDoc(c, acctB, store.KindNoteFull, "k")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func testDocsDeleteByKey(t *testing.T, s store.Store) {
	c := ctx(t)
	for _, key := range []string{"k1", "k2", "k3"} {
		require.NoError(t, s.PutDoc(c, acctA, store.KindNote, key, store.Doc{
			Payload: raw(`{"k":%q}`, key), FetchedAt: time.Now(),
		}))
	}

	require.NoError(t, s.DeleteDocs(c, acctA, store.KindNote, "k1", "k3"))

	_, err := s.GetDoc(c, acctA, store.KindNote, "k1")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetDoc(c, acctA, store.KindNote, "k3")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetDoc(c, acctA, store.KindNote, "k2")
	require.NoError(t, err, "delete must touch only the named keys")
}

func testDocsDeleteWholeKind(t *testing.T, s store.Store) {
	c := ctx(t)
	now := time.Now()
	require.NoError(t, s.PutDoc(c, acctA, store.KindNotifications, "mentions:20", store.Doc{Payload: raw(`{}`), FetchedAt: now}))
	require.NoError(t, s.PutDoc(c, acctA, store.KindNotifications, "likes:20", store.Doc{Payload: raw(`{}`), FetchedAt: now}))
	require.NoError(t, s.PutDoc(c, acctA, store.KindUnread, "count", store.Doc{Payload: raw(`{}`), FetchedAt: now}))
	require.NoError(t, s.PutDoc(c, acctB, store.KindNotifications, "mentions:20", store.Doc{Payload: raw(`{}`), FetchedAt: now}))

	// No keys means the whole kind for that account. This is how a write
	// invalidates every notification listing at once without enumerating the
	// tab/limit combinations that might have been cached.
	require.NoError(t, s.DeleteDocs(c, acctA, store.KindNotifications))

	for _, key := range []string{"mentions:20", "likes:20"} {
		_, err := s.GetDoc(c, acctA, store.KindNotifications, key)
		require.ErrorIs(t, err, store.ErrNotFound)
	}
	_, err := s.GetDoc(c, acctA, store.KindUnread, "count")
	require.NoError(t, err, "other kinds of the same account survive")
	_, err = s.GetDoc(c, acctB, store.KindNotifications, "mentions:20")
	require.NoError(t, err, "other accounts survive")
}

func testDocsEmptyMeta(t *testing.T, s store.Store) {
	c := ctx(t)
	require.NoError(t, s.PutDoc(c, acctA, store.KindFeed, "home", store.Doc{
		Payload: raw(`{"items":[]}`), FetchedAt: time.Now(),
	}))

	got, err := s.GetDoc(c, acctA, store.KindFeed, "home")
	require.NoError(t, err)
	// A caller with no meta to record must still get valid JSON back, so that
	// an accept predicate can unmarshal unconditionally.
	require.JSONEq(t, `{}`, string(got.Meta))
}

func notification(tab, id string) store.NotificationRecord {
	return store.NotificationRecord{
		Tab:            tab,
		NotificationID: id,
		FromUserID:     "u-" + id,
		NoteID:         "note-" + id,
		CommentID:      "c-" + id,
		Payload:        raw(`{"id":%q,"tab":%q}`, id, tab),
	}
}

func testNotificationsAppendDedup(t *testing.T, s store.Store) {
	c := ctx(t)

	n, err := s.AppendNotifications(c, acctA, nil)
	require.NoError(t, err)
	require.Equal(t, 0, n, "an empty append is a no-op, not an error")

	n, err = s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "1"),
		notification("mentions", "2"),
		notification("mentions", "3"),
	})
	require.NoError(t, err)
	require.Equal(t, 3, n)

	// The same listing fetched again must report nothing new. This count is
	// what the "new since last sync" answer is built on, so an off-by-one here
	// is a lie to the agent, not a performance detail.
	n, err = s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "1"),
		notification("mentions", "2"),
		notification("mentions", "3"),
	})
	require.NoError(t, err)
	require.Equal(t, 0, n)

	n, err = s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "3"),
		notification("mentions", "4"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n, "only genuinely new rows count")

	// A duplicate inside one batch counts once. The site does repeat items
	// across an overlapping page boundary.
	n, err = s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "5"),
		notification("mentions", "5"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	items, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, items, 5, "a limit of zero means no limit")
}

func testNotificationsTabIdentity(t *testing.T, s store.Store) {
	c := ctx(t)

	// Notification ids are not confirmed unique across tabs, so the tab is
	// part of the identity. Two rows here, not one.
	n, err := s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "same-id"),
		notification("likes", "same-id"),
	})
	require.NoError(t, err)
	require.Equal(t, 2, n)

	mentions, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, mentions, 1)
	require.JSONEq(t, `{"id":"same-id","tab":"mentions"}`, string(mentions[0].Payload))

	likes, err := s.NotificationsSince(c, acctA, "likes", "", 0)
	require.NoError(t, err)
	require.Len(t, likes, 1)
	require.JSONEq(t, `{"id":"same-id","tab":"likes"}`, string(likes[0].Payload))
}

func testNotificationsPagination(t *testing.T, s store.Store) {
	c := ctx(t)

	var batch []store.NotificationRecord
	for i := 0; i < 6; i++ {
		batch = append(batch, notification("mentions", fmt.Sprintf("%d", i)))
	}
	n, err := s.AppendNotifications(c, acctA, batch)
	require.NoError(t, err)
	require.Equal(t, 6, n)

	all, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, all, 6)

	// Insertion order, oldest first, with strictly increasing cursors. The
	// cursor is insertion order and not a site timestamp precisely so that the
	// site returning items out of order cannot make a caller skip one.
	for i := 1; i < len(all); i++ {
		require.Less(t, string(all[i-1].Cursor), string(all[i].Cursor),
			"cursors must be unique and ordered by byte value, so a caller can compare stored ones")
	}
	for i, it := range all {
		require.JSONEq(t, fmt.Sprintf(`{"id":"%d","tab":"mentions"}`, i), string(it.Payload))
		require.False(t, it.FirstSeen.IsZero(), "FirstSeen is set by the store on insert")
	}

	// Walking the stream a page at a time must visit every row exactly once.
	var walked []string
	cursor := store.Cursor("")
	for {
		page, err := s.NotificationsSince(c, acctA, "mentions", cursor, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		require.LessOrEqual(t, len(page), 2, "limit is a hard cap")
		for _, it := range page {
			walked = append(walked, string(it.Payload))
		}
		cursor = page[len(page)-1].Cursor
	}
	require.Len(t, walked, 6)

	// And the same read repeated is stable: no shifting window, no duplicates.
	again, err := s.NotificationsSince(c, acctA, "mentions", all[2].Cursor, 0)
	require.NoError(t, err)
	require.Len(t, again, 3)
	require.Equal(t, all[3].Cursor, again[0].Cursor)

	// A cursor at the end of the stream yields an empty page, not an error.
	tail, err := s.NotificationsSince(c, acctA, "mentions", all[5].Cursor, 0)
	require.NoError(t, err)
	require.Empty(t, tail)

	// Nothing has been fetched for this tab, so there is no stream at all.
	none, err := s.NotificationsSince(c, acctA, "never-fetched", "", 0)
	require.NoError(t, err)
	require.Empty(t, none, "an empty stream is an empty slice and a nil error")
}

func testNotificationsCursorsStable(t *testing.T, s store.Store) {
	c := ctx(t)

	_, err := s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "1"),
		notification("mentions", "2"),
	})
	require.NoError(t, err)

	before, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, before, 2)
	mark := before[1].Cursor

	// Re-appending an item already in the log must not move it. If a re-fetch
	// bumped a row's cursor, every item would reappear as "new" on every sync
	// and the whole point of the history log would be lost.
	_, err = s.AppendNotifications(c, acctA, []store.NotificationRecord{
		notification("mentions", "1"),
		notification("mentions", "2"),
		notification("mentions", "3"),
	})
	require.NoError(t, err)

	after, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, after, 3)
	require.Equal(t, before[0].Cursor, after[0].Cursor)
	require.Equal(t, before[1].Cursor, after[1].Cursor)
	require.Equal(t, before[0].FirstSeen, after[0].FirstSeen, "first seen means first seen")

	fresh, err := s.NotificationsSince(c, acctA, "mentions", mark, 0)
	require.NoError(t, err)
	require.Len(t, fresh, 1, "only the genuinely new row is past the stored cursor")
	require.JSONEq(t, `{"id":"3","tab":"mentions"}`, string(fresh[0].Payload))
}

func testNotificationsScoping(t *testing.T, s store.Store) {
	c := ctx(t)

	_, err := s.AppendNotifications(c, acctA, []store.NotificationRecord{notification("mentions", "1")})
	require.NoError(t, err)
	_, err = s.AppendNotifications(c, acctB, []store.NotificationRecord{notification("mentions", "1")})
	require.NoError(t, err)

	// Two accounts, two separate inboxes, even for the same notification id.
	// This is the failure the user-id scoping decision exists to prevent.
	a, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, a, 1)
	b, err := s.NotificationsSince(c, acctB, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, b, 1)
}

func testCommentsAppendAndRead(t *testing.T, s store.Store) {
	c := ctx(t)

	comment := func(noteID, id, parent string) store.CommentRecord {
		return store.CommentRecord{
			NoteID:    noteID,
			CommentID: id,
			ParentID:  parent,
			AuthorID:  "author-" + id,
			Payload:   raw(`{"id":%q,"parent":%q}`, id, parent),
		}
	}

	n, err := s.AppendComments(c, acctA, []store.CommentRecord{
		comment("note-1", "c1", ""),
		comment("note-1", "c2", "c1"), // a reply is its own row, not nested
		comment("note-2", "c3", ""),
	})
	require.NoError(t, err)
	require.Equal(t, 3, n)

	n, err = s.AppendComments(c, acctA, []store.CommentRecord{
		comment("note-1", "c1", ""),
		comment("note-1", "c9", "c1"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Scoped to one note, ordered, cursor-resumable — same contract as
	// notifications, different scope column.
	items, err := s.CommentsSince(c, acctA, "note-1", "", 0)
	require.NoError(t, err)
	require.Len(t, items, 3)
	for i := 1; i < len(items); i++ {
		require.Less(t, string(items[i-1].Cursor), string(items[i].Cursor))
	}

	rest, err := s.CommentsSince(c, acctA, "note-1", items[0].Cursor, 0)
	require.NoError(t, err)
	require.Len(t, rest, 2)

	other, err := s.CommentsSince(c, acctA, "note-2", "", 0)
	require.NoError(t, err)
	require.Len(t, other, 1)

	empty, err := s.CommentsSince(c, acctB, "note-1", "", 0)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func testAccountsUpsert(t *testing.T, s store.Store) {
	c := ctx(t)

	first := time.Now().Add(-48 * time.Hour)
	require.NoError(t, s.UpsertAccount(c, store.Account{
		AccountID: "user-1", Seed: 111, Nickname: "old",
		FirstSeen: first, LastSeen: first,
	}))

	later := time.Now()
	require.NoError(t, s.UpsertAccount(c, store.Account{
		AccountID: "user-1", Seed: 222, Nickname: "new",
		FirstSeen: later, LastSeen: later,
	}))

	got, err := s.AccountBySeed(c, 222)
	require.NoError(t, err)
	require.Equal(t, "user-1", got.AccountID)
	require.Equal(t, 222, got.Seed)
	require.Equal(t, "new", got.Nickname)
	require.WithinDuration(t, later, got.LastSeen, time.Microsecond)
	// First seen is written once. An upsert updates current state; it does not
	// rewrite history.
	require.WithinDuration(t, first, got.FirstSeen, time.Microsecond)

	// The seed moved with the account, so the old seed no longer resolves.
	// #6 keeps one seed across a re-login, so the mapping seed -> account is
	// many-to-one over time and only the current one may answer.
	_, err = s.AccountBySeed(c, 111)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func testAccountsBySeed(t *testing.T, s store.Store) {
	c := ctx(t)

	_, err := s.AccountBySeed(c, 999)
	require.ErrorIs(t, err, store.ErrNotFound, "an unknown seed is a miss, not an empty account")

	base := time.Now().Add(-time.Hour)
	require.NoError(t, s.UpsertAccount(c, store.Account{
		AccountID: "user-old", Seed: 7, LastSeen: base,
	}))
	require.NoError(t, s.UpsertAccount(c, store.Account{
		AccountID: "user-new", Seed: 7, LastSeen: base.Add(30 * time.Minute),
	}))
	require.NoError(t, s.UpsertAccount(c, store.Account{
		AccountID: "user-other", Seed: 8, LastSeen: base.Add(time.Hour),
	}))

	// Two accounts have shared this device profile. Startup recovery wants the
	// one that was driving it most recently.
	got, err := s.AccountBySeed(c, 7)
	require.NoError(t, err)
	require.Equal(t, "user-new", got.AccountID)

	got, err = s.AccountBySeed(c, 8)
	require.NoError(t, err)
	require.Equal(t, "user-other", got.AccountID)

	// Zero times are the store's problem, not the caller's: an account
	// observed without explicit timestamps must still be findable.
	require.NoError(t, s.UpsertAccount(c, store.Account{AccountID: "user-zero", Seed: 42}))
	got, err = s.AccountBySeed(c, 42)
	require.NoError(t, err)
	require.Equal(t, "user-zero", got.AccountID)
	require.False(t, got.FirstSeen.IsZero())
	require.False(t, got.LastSeen.IsZero())
}

func testPruneByAge(t *testing.T, s store.Store) {
	c := ctx(t)

	require.NoError(t, s.PutDoc(c, acctA, store.KindNote, "stale", store.Doc{
		Payload: raw(`{"v":"stale"}`), FetchedAt: time.Now().Add(-72 * time.Hour),
	}))
	require.NoError(t, s.PutDoc(c, acctB, store.KindProfile, "also-stale", store.Doc{
		Payload: raw(`{"v":"stale"}`), FetchedAt: time.Now().Add(-49 * time.Hour),
	}))
	require.NoError(t, s.PutDoc(c, acctA, store.KindNote, "fresh", store.Doc{
		Payload: raw(`{"v":"fresh"}`), FetchedAt: time.Now().Add(-time.Minute),
	}))

	// Retention is by age across every account and kind, driven by FetchedAt
	// and nothing else. There is no expires_at column to get out of step.
	n, err := s.PruneDocs(c, 48*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)

	_, err = s.GetDoc(c, acctA, store.KindNote, "stale")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetDoc(c, acctB, store.KindProfile, "also-stale")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetDoc(c, acctA, store.KindNote, "fresh")
	require.NoError(t, err)

	// Pruning again finds nothing left to do.
	n, err = s.PruneDocs(c, 48*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

func testPruneNonPositiveIsNoop(t *testing.T, s store.Store) {
	c := ctx(t)
	require.NoError(t, s.PutDoc(c, acctA, store.KindNote, "n1", store.Doc{
		Payload: raw(`{}`), FetchedAt: time.Now().Add(-time.Hour),
	}))

	// A zero or negative retention is a misconfiguration — an unset env var
	// parsed into a zero duration — and must not be read as "everything is
	// older than now".
	for _, d := range []time.Duration{0, -time.Hour} {
		n, err := s.PruneDocs(c, d)
		require.NoError(t, err)
		require.Equal(t, int64(0), n)
	}
	_, err := s.GetDoc(c, acctA, store.KindNote, "n1")
	require.NoError(t, err)
}

func testPruneLeavesHistory(t *testing.T, s store.Store) {
	c := ctx(t)

	_, err := s.AppendNotifications(c, acctA, []store.NotificationRecord{notification("mentions", "1")})
	require.NoError(t, err)
	_, err = s.AppendComments(c, acctA, []store.CommentRecord{{
		NoteID: "note-1", CommentID: "c1", Payload: raw(`{}`),
	}})
	require.NoError(t, err)

	// Retention applies to the cache, which can always be refetched. History
	// cannot: once the site stops showing a notification it is gone.
	_, err = s.PruneDocs(c, time.Nanosecond)
	require.NoError(t, err)

	notes, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	comments, err := s.CommentsSince(c, acctA, "note-1", "", 0)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func testCloseIdempotent(t *testing.T, s store.Store) {
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "shutdown paths must not have to track whether Close already ran")
}

func testConcurrency(t *testing.T, s store.Store) {
	c := ctx(t)

	// The store is shared by every in-flight MCP call. This is not a stress
	// test; it is here so that -race has something to look at.
	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", w)
			for i := 0; i < 20; i++ {
				assert.NoError(t, s.PutDoc(c, acctA, store.KindNote, key, store.Doc{
					Payload: raw(`{"w":%d,"i":%d}`, w, i), FetchedAt: time.Now(),
				}))
				if _, err := s.GetDoc(c, acctA, store.KindNote, key); err != nil {
					assert.ErrorIs(t, err, store.ErrNotFound)
				}
				_, err := s.AppendNotifications(c, acctA, []store.NotificationRecord{
					notification("mentions", fmt.Sprintf("%d-%d", w, i)),
				})
				assert.NoError(t, err)
				_, err = s.NotificationsSince(c, acctA, "mentions", "", 5)
				assert.NoError(t, err)
				assert.NoError(t, s.UpsertAccount(c, store.Account{
					AccountID: fmt.Sprintf("user-%d", w), Seed: 1, LastSeen: time.Now(),
				}))
			}
		}(w)
	}
	wg.Wait()

	items, err := s.NotificationsSince(c, acctA, "mentions", "", 0)
	require.NoError(t, err)
	require.Len(t, items, workers*20, "every concurrent append landed exactly once")
	seen := map[store.Cursor]bool{}
	for i, it := range items {
		require.False(t, seen[it.Cursor], "cursors must be unique under concurrency")
		seen[it.Cursor] = true
		if i > 0 {
			require.Less(t, string(items[i-1].Cursor), string(it.Cursor))
		}
	}

	_, err = s.AccountBySeed(c, 1)
	require.NoError(t, err)

	// Errors reported from goroutines above are assert.* on purpose: a
	// require inside a goroutine would call runtime.Goexit and hang the wait.
	if t.Failed() {
		t.FailNow()
	}
}
