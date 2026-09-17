package xiaohongshu

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNoteLinks(t *testing.T) {
	const id = "65a1b2c3d4e5f60718293a4b"

	cases := []struct {
		name  string
		hrefs []string
		want  []noteLink
	}{
		{
			name:  "takes the source the site put in the href",
			hrefs: []string{"/explore/" + id + "?xsec_token=ABC&xsec_source=pc_notification"},
			want:  []noteLink{{id: id, source: "pc_notification"}},
		},
		{
			name:  "absolute urls and the discovery path shape",
			hrefs: []string{"https://www.xiaohongshu.com/discovery/item/" + id + "?xsec_token=ABC&xsec_source=pc_user"},
			want:  []noteLink{{id: id, source: "pc_user"}},
		},
		{
			// A link with no source teaches us nothing, and recording a blank
			// would replace a good record with no record.
			name:  "a link without xsec_source is ignored",
			hrefs: []string{"/explore/" + id + "?xsec_token=ABC"},
			want:  nil,
		},
		{
			name: "non-note links are ignored",
			hrefs: []string{
				"/user/profile/5f8?xsec_token=ABC&xsec_source=pc_note",
				"/notification?xsec_source=pc_feed",
				"https://creator.xiaohongshu.com/publish/publish?xsec_source=pc_feed",
			},
			want: nil,
		},
		{
			name: "the first record of a note wins and duplicates are dropped",
			hrefs: []string{
				"/explore/" + id + "?xsec_source=pc_notification",
				"/explore/" + id + "?xsec_source=pc_feed",
			},
			want: []noteLink{{id: id, source: "pc_notification"}},
		},
		{
			name:  "an id that is not note-shaped is not a note",
			hrefs: []string{"/explore/not-a-note-id?xsec_source=pc_feed"},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNoteLinks(tc.hrefs)
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				assert.Equal(t, tc.want[i], got[i])
			}
		})
	}
}

// TestNoteSourceFromNotificationLink is the point of the exercise: once the
// notification page has told us where its notes come from, opening one no
// longer falls back to claiming it came from the feed.
func TestNoteSourceFromNotificationLink(t *testing.T) {
	ctx := context.Background()
	const id = "65a1b2c3d4e5f60718293a4c"

	SetNoteSources(nil)
	t.Cleanup(func() { SetNoteSources(nil) })

	source, referrer := feedEntryPoint(ctx, id)
	require.Equal(t, xsecSourceFeed, source, "with no record, the honest default")
	require.Equal(t, urlExplore, referrer)

	for _, link := range parseNoteLinks([]string{"/explore/" + id + "?xsec_token=ABC&xsec_source=pc_notification"}) {
		rememberNoteSource(ctx, link.id, link.source, urlNotification)
	}

	source, referrer = feedEntryPoint(ctx, id)
	assert.Equal(t, "pc_notification", source)
	assert.Equal(t, urlNotification, referrer)
}
