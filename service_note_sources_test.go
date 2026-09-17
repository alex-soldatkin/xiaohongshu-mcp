package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store/memstore"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// WS4: note provenance in the store. memstore implements the optional
// capability; the counting fake in service_cache_test.go deliberately does
// not, which is what keeps the fallback path exercised.

func newNoteSourceTestCache(t *testing.T, st store.Store) *serviceCache {
	t.Helper()

	c := newServiceCache(st)
	c.cfg = cacheConfig{TTLs: defaultCacheTTLs(), Retention: 0}
	return c
}

func TestStoreNoteSourcesRoundTrip(t *testing.T) {
	st := memstore.New()
	cache := newNoteSourceTestCache(t, st)
	cache.setAccount("acct-1")

	ns := newStoreNoteSources(cache)
	require.NotNil(t, ns)

	ctx := context.Background()
	const referrer = "https://www.xiaohongshu.com/search_result?keyword=go"
	ns.Remember(ctx, "note-1", "pc_search", referrer)

	source, gotReferrer, ok := ns.Lookup(ctx, "note-1")
	require.True(t, ok)
	assert.Equal(t, "pc_search", source)
	assert.Equal(t, referrer, gotReferrer)

	// It really is in the store, which is the whole point: the record now
	// outlives the process that observed it.
	src, err := st.LookupNoteSource(ctx, "acct-1", "note-1", 0)
	require.NoError(t, err)
	assert.Equal(t, "pc_search", src.Source)

	_, _, ok = ns.Lookup(ctx, "never-seen")
	assert.False(t, ok)
}

// A record past the provenance window is refused by the store's own query, so
// the claim falls back to the honest default rather than naming a page that is
// long gone.
func TestStoreNoteSourcesRespectsTheWindow(t *testing.T) {
	st := memstore.New()
	cache := newNoteSourceTestCache(t, st)
	cache.setAccount("acct-1")
	ns := newStoreNoteSources(cache)

	ctx := context.Background()
	require.NoError(t, st.RememberNoteSource(ctx, "acct-1", store.NoteSource{
		FeedID: "note-1", Source: "pc_search", SeenAt: time.Now().Add(-xiaohongshu.NoteSourceTTL - time.Minute),
	}))

	_, _, ok := ns.Lookup(ctx, "note-1")
	assert.False(t, ok)
}

// The window in which the account is not yet known is not hypothetical: on a
// fresh database the first listing is remembered inside the browser action,
// and the account is only observed once that action has returned. The local
// table covers exactly that, and the store takes over as soon as there is an
// account to key rows by.
func TestStoreNoteSourcesFallsBackBeforeTheAccountIsKnown(t *testing.T) {
	st := memstore.New()
	cache := newNoteSourceTestCache(t, st)
	ns := newStoreNoteSources(cache)
	require.NotNil(t, ns)

	ctx := context.Background()
	ns.Remember(ctx, "note-1", "pc_feed", "https://www.xiaohongshu.com/explore")

	source, _, ok := ns.Lookup(ctx, "note-1")
	require.True(t, ok, "provenance recorded before the account was known is still usable")
	assert.Equal(t, "pc_feed", source)

	_, err := st.LookupNoteSource(ctx, "", "note-1", 0)
	assert.ErrorIs(t, err, store.ErrNotFound, "nothing is written under an empty account")

	// Once the account is known, the store is where new records go.
	cache.setAccount("acct-1")
	ns.Remember(ctx, "note-2", "pc_search", "https://www.xiaohongshu.com/search_result?keyword=go")
	src, err := st.LookupNoteSource(ctx, "acct-1", "note-2", 0)
	require.NoError(t, err)
	assert.Equal(t, "pc_search", src.Source)

	// And the earlier record is still answered, from the local table.
	source, _, ok = ns.Lookup(ctx, "note-1")
	require.True(t, ok)
	assert.Equal(t, "pc_feed", source)
}

// Without a store, or with a backend that does not offer the capability, the
// xiaohongshu package keeps its own table and nothing is installed at all.
func TestStoreNoteSourcesNotInstalledWithoutTheCapability(t *testing.T) {
	assert.Nil(t, newStoreNoteSources(newServiceCache(nil)),
		"no store means no seam replacement")
	assert.Nil(t, newStoreNoteSources(newServiceCache(store.Nop{})))

	// The counting fake is a complete store.Store without NoteSourceStore.
	fake := &fakeStore{inner: memstore.New()}
	cache := newNoteSourceTestCache(t, fake)
	assert.Nil(t, cache.noteSources, "the capability is type-asserted, not assumed")
	assert.Nil(t, newStoreNoteSources(cache))
	assert.Equal(t, 0, fake.callCount())
}
