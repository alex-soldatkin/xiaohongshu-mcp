//go:build integration

// Integration test: launches a browser plus a local HTTP server. A plain go
// test neither builds nor runs it.
// Run it by hand: GOARCH=arm64 go test -tags integration ./xiaohongshu/ -run TestClickThrough
package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// siteRecorder is a one-origin stand-in for the main site: a few surfaces that
// may or may not contain the sidebar link, and a destination that remembers the
// Referer of the document request that reached it.
type siteRecorder struct {
	*httptest.Server
	mu    sync.Mutex
	refer map[string]string
}

func newSiteRecorder(t *testing.T) *siteRecorder {
	t.Helper()

	s := &siteRecorder{refer: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Document requests only. The favicon fetch that follows a navigation
		// carries a Referer of its own — the page just opened — and recording
		// it would turn every assertion below into a tautology that passes
		// against an implementation which sends no referrer at all.
		if req.Header.Get("Sec-Fetch-Dest") == "document" {
			s.mu.Lock()
			s.refer[req.URL.Path] = req.Header.Get("Referer")
			s.mu.Unlock()
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sitePage(req.URL.Path))
	}))
	t.Cleanup(s.Close)
	return s
}

// referer returns the Referer carried by the document request that reached
// path. If no such request was made, the test itself went off the rails.
func (s *siteRecorder) referer(t *testing.T, path string) string {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	got, ok := s.refer[path]
	require.True(t, ok, "no document request was ever served for %s", path)
	return got
}

// Surfaces served by siteRecorder. The explore variants differ only in the
// sidebar entry, which is exactly the difference click-through has to notice.
const (
	pathSidebar   = "/explore"           // sidebar links to the destination
	pathNoLink    = "/explore-no-link"   // sidebar has no such entry
	pathBlankLink = "/explore-new-tab"   // the entry is there but opens a new tab
	pathOtherLink = "/explore-elsewhere" // the entry points somewhere else
	pathDest      = "/notification"      // the destination
	pathFallback  = "/fallback"          // referrer claimed by the direct navigation
)

// sitePage renders a surface. The markup mirrors the real sidebar closely
// enough for the production selectors to apply to it.
func sitePage(path string) string {
	var entry string
	switch path {
	case pathSidebar:
		entry = `<a class="link-wrapper" href="/notification"><span class="channel">通知</span></a>`
	case pathBlankLink:
		entry = `<a class="link-wrapper" href="/notification" target="_blank"><span class="channel">通知</span></a>`
	case pathOtherLink:
		entry = `<a class="link-wrapper" href="/somewhere-else"><span class="channel">通知</span></a>`
	}

	return `<!doctype html><html lang="zh-CN"><body>
	<div id="app"><div class="main-container"><ul>
	<li class="user side-bar-component"><a class="link-wrapper" href="/user/profile/me"><span class="channel">我</span></a></li>
	<li class="side-bar-component">` + entry + `</li>
	</ul><p id="ok">ok</p></div></div></body></html>`
}

// testTarget is the production notification target pointed at the local origin.
// The fallback referrer is deliberately a different page from the surface, so
// the recorded Referer says which of the two routes ran.
func testTarget(site *siteRecorder) clickTarget {
	t := notificationTarget()
	t.dest = site.URL + pathDest
	t.referrer = site.URL + pathFallback
	t.surface = func(current string) bool { return strings.HasPrefix(current, site.URL+"/") }
	return t
}

// TestClickThroughSendsReferrer is the acceptance gate for the second half of
// #10: when the surface really does contain the link, we click it, and the
// destination sees the surface as its referrer — not a claim we made up, but
// the address the browser itself was on.
func TestClickThroughSendsReferrer(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })

	b := browser.NewBrowser(true)
	defer b.Close()

	site := newSiteRecorder(t)
	page := b.NewPage()
	defer func() { _ = page.Close() }()

	ctx := context.Background()
	require.NoError(t, navigateFrom(ctx, page, site.URL+pathSidebar, "", navWaitLoad))

	target := testTarget(site)
	done, err := clickThrough(ctx, page, target)
	require.NoError(t, err)
	require.True(t, done, "the sidebar link was present and should have been clicked")

	assert.Equal(t, site.URL+pathDest, currentURL(page))
	assert.Equal(t, site.URL+pathSidebar, site.referer(t, pathDest),
		"Referer header on the document request")
	assert.Equal(t, site.URL+pathSidebar, page.MustEval(`() => document.referrer`).String(),
		"document.referrer")
}

// TestClickThroughFallsBack is the constraint that matters more than the
// realism: every way a click-through can fail has to end with the caller on the
// destination anyway, reached by a direct navigation, with no error surfaced.
func TestClickThroughFallsBack(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })

	cases := []struct {
		name    string
		surface string
		why     string
	}{
		{"link absent", pathNoLink, "the sidebar has no entry for this destination"},
		{"link opens a new tab", pathBlankLink, "clicking would navigate a tab we do not hold"},
		{"link points elsewhere", pathOtherLink, "the href does not lead to the destination"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := browser.NewBrowser(true)
			defer b.Close()

			site := newSiteRecorder(t)
			page := b.NewPage()
			defer func() { _ = page.Close() }()

			before := len(b.Rod().MustPages())

			ctx := context.Background()
			require.NoError(t, navigateFrom(ctx, page, site.URL+tc.surface, "", navWaitLoad))

			target := testTarget(site)

			done, err := clickThrough(ctx, page, target)
			require.NoError(t, err, tc.why)
			require.False(t, done, tc.why)

			// openTarget is what call sites use: the fallback has to be
			// automatic, so this must land on the destination on its own.
			require.NoError(t, openTarget(ctx, page, target))

			assert.Equal(t, site.URL+pathDest, currentURL(page))
			assert.Equal(t, site.URL+pathFallback, site.referer(t, pathDest),
				"the direct navigation's referrer, proving the click-through did not run")
			assert.Len(t, b.Rod().MustPages(), before, "no stray tab was opened")
		})
	}
}

// TestClickThroughSkipsForeignSurface pins the other half of the cheapness
// rule: click-through replaces a navigation, it never adds one. Off the main
// site there is no sidebar to click, and we must not load one to get at it.
func TestClickThroughSkipsForeignSurface(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })

	b := browser.NewBrowser(true)
	defer b.Close()

	site := newSiteRecorder(t)
	elsewhere := newRefRecorder(t)

	page := b.NewPage()
	defer func() { _ = page.Close() }()

	ctx := context.Background()
	require.NoError(t, navigateFrom(ctx, page, elsewhere.URL+"/somewhere", "", navWaitLoad))

	done, err := clickThrough(ctx, page, testTarget(site))
	require.NoError(t, err)
	assert.False(t, done, "a page off the surface must not be clicked through")
}
