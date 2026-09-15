package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
)

// TestNopMissesEverything pins the one behaviour the unset-database deployment
// depends on: the no-op store answers every read with a miss and swallows
// every write, so the cache layer above it degrades to today's code path
// instead of erroring.
//
// The no-op store is not run through storetest.Run: it fails the contract by
// design, since nothing it is told to store comes back.
func TestNopMissesEverything(t *testing.T) {
	c := context.Background()
	var s store.Store = store.Nop{}

	require.NoError(t, s.PutDoc(c, "acct", store.KindNote, "n1", store.Doc{
		Payload: []byte(`{"v":1}`), FetchedAt: time.Now(),
	}))
	_, err := s.GetDoc(c, "acct", store.KindNote, "n1")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, s.DeleteDocs(c, "acct", store.KindNote))
	require.NoError(t, s.DeleteDocs(c, "acct", store.KindNote, "n1"))

	n, err := s.AppendNotifications(c, "acct", []store.NotificationRecord{{Tab: "mentions", NotificationID: "1"}})
	require.NoError(t, err)
	require.Zero(t, n)
	items, err := s.NotificationsSince(c, "acct", "mentions", "", 0)
	require.NoError(t, err)
	require.Empty(t, items)

	n, err = s.AppendComments(c, "acct", []store.CommentRecord{{NoteID: "n1", CommentID: "c1"}})
	require.NoError(t, err)
	require.Zero(t, n)
	comments, err := s.CommentsSince(c, "acct", "n1", "", 0)
	require.NoError(t, err)
	require.Empty(t, comments)

	require.NoError(t, s.UpsertAccount(c, store.Account{AccountID: "user-1", Seed: 1}))
	_, err = s.AccountBySeed(c, 1)
	require.ErrorIs(t, err, store.ErrNotFound)

	pruned, err := s.PruneDocs(c, time.Hour)
	require.NoError(t, err)
	require.Zero(t, pruned)

	require.NoError(t, s.Close())
}
