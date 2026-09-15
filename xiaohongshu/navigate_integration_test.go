//go:build integration

// 集成测试：起浏览器 + 本地 HTTP 服务，默认 go test 不编译不运行。
// 手动跑：GOARCH=arm64 go test -tags integration ./xiaohongshu/ -run TestNavigateFrom
package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// fastTiming keeps the human-scale pauses out of the test's wall clock. The
// pauses are not what is under test here; the referrer is.
type fastTiming struct{}

func (fastTiming) Timing() humanize.TimingProfile {
	profile := humanize.TimingProfile{}
	for action, dist := range (humanize.DefaultProvider{}).Timing() {
		dist.Min, dist.Max, dist.Mu, dist.Sigma = time.Millisecond, 2*time.Millisecond, -7, 0
		profile[action] = dist
	}
	return profile
}

// refRecorder is a target origin that remembers the Referer of the last
// document request it served.
type refRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	got  string
	seen bool
}

func newRefRecorder(t *testing.T) *refRecorder {
	t.Helper()

	r := &refRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Only the top-level document request counts. The favicon fetch that
		// follows it carries a Referer of its own — the page we just opened —
		// and would overwrite the answer with a tautology.
		if req.Header.Get("Sec-Fetch-Dest") == "document" {
			r.mu.Lock()
			r.got = req.Header.Get("Referer")
			r.seen = true
			r.mu.Unlock()
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><p id="ok">ok</p></body></html>`)
	}))
	t.Cleanup(r.Close)
	return r
}

// referer 返回文档请求上的 Referer；没收到文档请求就是测试本身跑歪了。
func (r *refRecorder) referer(t *testing.T) string {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	require.True(t, r.seen, "server never served a document request")
	return r.got
}

func (r *refRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got, r.seen = "", false
}

// TestNavigateFromSendsReferrer is the acceptance gate for #10: a deep link
// opened through navigateFrom must arrive with a Referer header on the document
// request and a matching document.referrer, instead of looking like a session
// that teleported there.
//
// It runs against two local origins rather than the live site so that it needs
// no login and the expected values are exact.
func TestNavigateFromSendsReferrer(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })

	b := browser.NewBrowser(true)
	defer b.Close()

	target := newRefRecorder(t)
	// A second origin (same scheme, different port) to exercise the cross-origin
	// trimming rule.
	other := newRefRecorder(t)

	cases := []struct {
		name string
		// referrer passed to navigateFrom
		referrer string
		// what the destination and document.referrer should end up seeing
		want string
	}{
		{
			name:     "same origin keeps the full URL",
			referrer: target.URL + "/explore?from=feed",
			want:     target.URL + "/explore?from=feed",
		},
		{
			// This is the creator.xiaohongshu.com case: the default
			// strict-origin-when-cross-origin policy trims the path away. The
			// site still learns we came from the main domain, which is the
			// point; a real browser behaves identically.
			name:     "cross origin is trimmed to the origin",
			referrer: other.URL + "/explore?from=feed",
			want:     other.URL + "/",
		},
		{
			// An empty referrer is the honest way to say "the user typed this",
			// which is what the entry-point navigations claim.
			name:     "no referrer sends nothing",
			referrer: "",
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target.reset()

			page := b.NewPage()
			defer func() { _ = page.Close() }()

			cdpReferer := captureDocumentReferer(page)

			url := target.URL + "/explore/abc123?xsec_token=tok&xsec_source=pc_feed"
			require.NoError(t, navigateFrom(context.Background(), page, url, tc.referrer, navWaitLoad))

			assert.Equal(t, tc.want, target.referer(t), "Referer header received by the server")
			assert.Equal(t, tc.want, cdpReferer(), "Referer header on the CDP document request")

			referrer := page.MustEval(`() => document.referrer`).String()
			assert.Equal(t, tc.want, referrer, "document.referrer")
		})
	}
}

// captureDocumentReferer subscribes to Network.requestWillBeSent and returns a
// reader for the Referer of the first document request.
func captureDocumentReferer(page *rod.Page) func() string {
	var (
		mu      sync.Mutex
		referer string
		seen    bool
	)

	wait := page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		if e.Type != proto.NetworkResourceTypeDocument {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if seen {
			return
		}
		seen = true
		// Header names are case-insensitive and Chrome is not consistent here.
		for name, value := range e.Request.Headers {
			if strings.EqualFold(name, "Referer") {
				referer = value.Str()
			}
		}
	})
	go wait()

	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return referer
	}
}

// TestNavigateFromRepairsJSContext guards the one thing navigateFrom cannot do
// that rod's Page.Navigate does: drop rod's cached JS execution context id.
// Evaluating on the page after navigating has to keep working.
func TestNavigateFromRepairsJSContext(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })

	b := browser.NewBrowser(true)
	defer b.Close()

	first := newRefRecorder(t)
	second := newRefRecorder(t)

	page := b.NewPage()
	defer func() { _ = page.Close() }()

	ctx := context.Background()
	require.NoError(t, navigateFrom(ctx, page, first.URL+"/a", "", navWaitLoad))
	require.Equal(t, "ok", page.MustEval(`() => document.querySelector("#ok").textContent`).String())

	require.NoError(t, navigateFrom(ctx, page, second.URL+"/b", first.URL+"/a", navWaitLoad))
	require.Equal(t, "ok", page.MustEval(`() => document.querySelector("#ok").textContent`).String())
	require.Equal(t, second.URL+"/b", currentURL(page))
}
