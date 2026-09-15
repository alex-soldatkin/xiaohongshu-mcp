package memstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store/memstore"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store/storetest"
)

// TestContract runs the shared contract suite against the in-memory backend.
// It is the same suite a Postgres or Mongo backend has to pass, so a change to
// the contract that memstore cannot satisfy is a change that has to be argued
// for here first.
func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return memstore.New() })
}

// TestSetClock covers the one thing memstore has beyond the contract: an
// injectable clock, so the cache layer's TTL and retention tests can age rows
// without sleeping.
func TestSetClock(t *testing.T) {
	c := context.Background()
	s := memstore.New()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	require.NoError(t, s.PutDoc(c, "acct", store.KindNote, "n1", store.Doc{Payload: []byte(`{}`)}))

	got, err := s.GetDoc(c, "acct", store.KindNote, "n1")
	require.NoError(t, err)
	require.True(t, got.FetchedAt.Equal(now), "a zero FetchedAt is filled from the clock")

	n, err := s.PruneDocs(c, 24*time.Hour)
	require.NoError(t, err)
	require.Zero(t, n)

	now = now.Add(48 * time.Hour)
	n, err = s.PruneDocs(c, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}
