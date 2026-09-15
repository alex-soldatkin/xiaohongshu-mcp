package xiaohongshu

import (
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

	// 空来源退回信息流，保持改动前的行为
	assert.Equal(t,
		"https://www.xiaohongshu.com/explore/abc?xsec_token=tok&xsec_source=pc_feed",
		makeFeedDetailURL("abc", "tok", ""))
}

func TestNoteSourceRoundTrip(t *testing.T) {
	table := newNoteSourceTable()
	table.rememberFeeds([]Feed{{ID: "n1"}, {ID: "n2"}}, xsecSourceSearch, "https://www.xiaohongshu.com/search_result?keyword=x")

	e, ok := table.lookup("n2")
	require.True(t, ok)
	assert.Equal(t, xsecSourceSearch, e.source)
	assert.Equal(t, "https://www.xiaohongshu.com/search_result?keyword=x", e.referrer)

	_, ok = table.lookup("unknown")
	assert.False(t, ok)
}

func TestNoteSourceExpires(t *testing.T) {
	now := time.Now()
	table := newNoteSourceTable()
	table.now = func() time.Time { return now }

	table.remember("n1", xsecSourceNote, urlExplore)

	now = now.Add(noteSourceTTL - time.Minute)
	_, ok := table.lookup("n1")
	assert.True(t, ok)

	now = now.Add(2 * time.Minute)
	_, ok = table.lookup("n1")
	assert.False(t, ok, "过期后不该再声称来源")
}

func TestNoteSourceStaysBounded(t *testing.T) {
	table := newNoteSourceTable()
	for i := 0; i < noteSourceCapacity*2; i++ {
		table.remember(fmt.Sprintf("n%d", i), xsecSourceFeed, urlExplore)
	}
	assert.LessOrEqual(t, len(table.entries), noteSourceCapacity)

	// 最新写入的必须还在
	_, ok := table.lookup(fmt.Sprintf("n%d", noteSourceCapacity*2-1))
	assert.True(t, ok)
}

func TestFeedEntryPointDefaultsToFeed(t *testing.T) {
	source, referrer := feedEntryPoint("never-seen-before")
	assert.Equal(t, xsecSourceFeed, source)
	assert.Equal(t, urlExplore, referrer)
}

func TestFeedEntryPointUsesRememberedOrigin(t *testing.T) {
	const id = "note-from-search"
	noteSources.remember(id, xsecSourceSearch, "https://www.xiaohongshu.com/search_result?keyword=go")
	t.Cleanup(func() { delete(noteSources.entries, id) })

	source, referrer := feedEntryPoint(id)
	assert.Equal(t, xsecSourceSearch, source)
	assert.Equal(t, "https://www.xiaohongshu.com/search_result?keyword=go", referrer)
}
