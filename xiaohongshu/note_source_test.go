package xiaohongshu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMakeFeedDetailURLCarriesSource(t *testing.T) {
	assert.Equal(t,
		"https://www.xiaohongshu.com/explore/abc?xsec_token=tok&xsec_source=pc_search",
		makeFeedDetailURL("abc", "tok", xsecSourceSearch))

	// An empty source falls back to the feed, preserving the previous behaviour.
	assert.Equal(t,
		"https://www.xiaohongshu.com/explore/abc?xsec_token=tok&xsec_source=pc_feed",
		makeFeedDetailURL("abc", "tok", ""))
}

func TestNoteSourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	table := newNoteSourceTable()
	for _, f := range []Feed{{ID: "n1"}, {ID: "n2"}} {
		table.Remember(ctx, f.ID, xsecSourceSearch, "https://www.xiaohongshu.com/search_result?keyword=x")
	}

	source, referrer, ok := table.Lookup(ctx, "n2")
	require.True(t, ok)
	assert.Equal(t, xsecSourceSearch, source)
	assert.Equal(t, "https://www.xiaohongshu.com/search_result?keyword=x", referrer)

	_, _, ok = table.Lookup(ctx, "unknown")
	assert.False(t, ok)
}

// The package-level helper is what the listing readers call; it must reach
// whatever implementation is installed, not a table captured at build time.
func TestRememberFeedSourcesUsesInstalledImplementation(t *testing.T) {
	ctx := context.Background()
	installed := newNoteSourceTable()
	SetNoteSources(installed)
	t.Cleanup(func() { SetNoteSources(nil) })

	rememberFeedSources(ctx, []Feed{{ID: "n1"}, {ID: "n2"}}, xsecSourceSearch, "https://www.xiaohongshu.com/search_result?keyword=x")

	source, _, ok := installed.Lookup(ctx, "n1")
	require.True(t, ok)
	assert.Equal(t, xsecSourceSearch, source)

	source, referrer := feedEntryPoint(ctx, "n2")
	assert.Equal(t, xsecSourceSearch, source)
	assert.Equal(t, "https://www.xiaohongshu.com/search_result?keyword=x", referrer)
}

// Passing nil restores the default, so a test or a reconfiguration cannot
// leave the package without an implementation.
func TestSetNoteSourcesNilRestoresDefault(t *testing.T) {
	SetNoteSources(nil)
	require.NotNil(t, currentNoteSources())

	source, referrer := feedEntryPoint(context.Background(), "never-seen-at-all")
	assert.Equal(t, xsecSourceFeed, source)
	assert.Equal(t, urlExplore, referrer)
}

func TestNoteSourceExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	table := newNoteSourceTable()
	table.now = func() time.Time { return now }

	table.Remember(ctx, "n1", xsecSourceNote, urlExplore)

	now = now.Add(NoteSourceTTL - time.Minute)
	_, _, ok := table.Lookup(ctx, "n1")
	assert.True(t, ok)

	now = now.Add(2 * time.Minute)
	_, _, ok = table.Lookup(ctx, "n1")
	assert.False(t, ok, "过期后不该再声称来源")
}

func TestNoteSourceStaysBounded(t *testing.T) {
	ctx := context.Background()
	table := newNoteSourceTable()
	for i := 0; i < noteSourceCapacity*2; i++ {
		table.Remember(ctx, fmt.Sprintf("n%d", i), xsecSourceFeed, urlExplore)
	}
	assert.LessOrEqual(t, len(table.entries), noteSourceCapacity)

	// The most recently written entry must still be there.
	_, _, ok := table.Lookup(ctx, fmt.Sprintf("n%d", noteSourceCapacity*2-1))
	assert.True(t, ok)
}

func TestFeedEntryPointDefaultsToFeed(t *testing.T) {
	source, referrer := feedEntryPoint(context.Background(), "never-seen-before")
	assert.Equal(t, xsecSourceFeed, source)
	assert.Equal(t, urlExplore, referrer)
}

func TestFeedEntryPointUsesRememberedOrigin(t *testing.T) {
	ctx := context.Background()
	const id = "note-from-search"

	table := newNoteSourceTable()
	SetNoteSources(table)
	t.Cleanup(func() { SetNoteSources(nil) })
	table.Remember(ctx, id, xsecSourceSearch, "https://www.xiaohongshu.com/search_result?keyword=go")

	source, referrer := feedEntryPoint(ctx, id)
	assert.Equal(t, xsecSourceSearch, source)
	assert.Equal(t, "https://www.xiaohongshu.com/search_result?keyword=go", referrer)
}
