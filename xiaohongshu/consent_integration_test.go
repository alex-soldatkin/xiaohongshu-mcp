//go:build integration

// Integration test: launches a browser plus a local HTTP server. A plain go
// test neither builds nor runs it.
// Run it by hand: GOARCH=arm64 go test -tags integration ./xiaohongshu/ -run TestConsent
package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// consentPage reproduces the measured rednote banner: a full-viewport fixed
// overlay at z-index 999998, the banner above it, and underneath both a link
// standing in for the sidebar. Clicking the link records the fact in the title,
// which is readable whether or not the click ever lands.
const consentPage = `<!doctype html><html lang="en"><head><title>untouched</title>
<style>
 body { margin: 0 }
 .cookie-banner-overlay { position: fixed; inset: 0; z-index: 999998; background: rgba(0,0,0,.4) }
 .cookie-banner { position: fixed; right: 20px; bottom: 20px; z-index: 999999; background: #fff; padding: 16px }
 #sidebar { position: absolute; left: 60px; top: 280px; width: 160px; height: 40px }
</style></head><body>
<div id="app">
  <a id="sidebar" href="#" onclick="document.title='clicked'; return false">我</a>
  <div class="cookie-banner-overlay"></div>
  <div class="cookie-banner cookie-banner--web">
    <h2>Your Cookie Preferences</h2>
    <div class="cookie-banner__actions">
      <button class="cookie-banner__btn cookie-banner__btn--primary" onclick="dismiss()">Accept all cookies</button>
      <button class="cookie-banner__btn cookie-banner__btn--secondary">Decline optional cookies</button>
    </div>
  </div>
</div>
<script>
 function dismiss() {
   localStorage.setItem('xhs_cookie_consent', JSON.stringify({hasConsented: true}));
   document.querySelector('.cookie-banner-overlay').remove();
   document.querySelector('.cookie-banner').remove();
 }
 // Second and later loads in the same profile behave like the real site: the
 // banner is never rendered once consent is stored.
 if (localStorage.getItem('xhs_cookie_consent')) { dismiss(); }
</script></body></html>`

func newConsentSite(t *testing.T) *httptest.Server {
	t.Helper()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, consentPage)
	}))
	t.Cleanup(s.Close)
	return s
}

// withConsentSite installs a deployment profile that has a banner, and puts the
// previous one back afterwards.
func withConsentSite(t *testing.T, selector string) {
	t.Helper()

	previous := ActiveSite()
	site := previous
	site.ConsentAcceptSelector = selector
	SetSite(site)
	t.Cleanup(func() { SetSite(previous) })
}

// TestConsentOverlayBlocksClicks is the premise the whole feature rests on: the
// overlay is not decoration, it eats the click. Without it, dismissal would be
// politeness rather than a requirement.
func TestConsentOverlayBlocksClicks(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })
	withConsentSite(t, "") // no dismissal: show what an unhandled banner does

	b := browser.NewBrowser(true)
	defer b.Close()

	site := newConsentSite(t)
	page := b.NewPage()
	defer func() { _ = page.Close() }()

	require.NoError(t, navigateFrom(context.Background(), page, site.URL+"/", "", navWaitLoad))

	topmost := page.MustEval(`() => {
		const r = document.querySelector('#sidebar').getBoundingClientRect();
		const el = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
		return el.className || el.id;
	}`).Str()
	assert.Equal(t, "cookie-banner-overlay", topmost,
		"the overlay should be the topmost element over the sidebar")

	// The real click follows suit: it reaches the overlay, and the link never
	// sees it. The timeout is the assertion: humanize.Click waits for the
	// element to become interactable, and behind the overlay it never does —
	// without a deadline this hangs, which is exactly what the tool would do
	// in production.
	link := page.Timeout(5 * time.Second).MustElement("#sidebar")
	err := humanize.Click(link)
	assert.Error(t, err, "a click behind the overlay should never become interactable")
	assert.Equal(t, "untouched", page.MustInfo().Title,
		"the sidebar link must not receive a click through the overlay")
}

// TestConsentDismissedOnNavigate is the fix: navigateFrom accepts the banner,
// and the same click then lands.
func TestConsentDismissedOnNavigate(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })
	withConsentSite(t, "button.cookie-banner__btn--primary")

	b := browser.NewBrowser(true)
	defer b.Close()

	site := newConsentSite(t)
	page := b.NewPage()
	defer func() { _ = page.Close() }()

	ctx := context.Background()
	require.NoError(t, navigateFrom(ctx, page, site.URL+"/", "", navWaitLoad))

	has, _, err := page.Has(".cookie-banner-overlay")
	require.NoError(t, err)
	assert.False(t, has, "the overlay should be gone after navigateFrom")

	link := page.MustElement("#sidebar")
	require.NoError(t, humanize.Click(link))
	assert.Equal(t, "clicked", page.MustInfo().Title)

	// Consent persists for the profile, so the second navigation finds nothing
	// to dismiss and must still succeed.
	require.NoError(t, navigateFrom(ctx, page, site.URL+"/", "", navWaitLoad))
	has, _, err = page.Has(".cookie-banner-overlay")
	require.NoError(t, err)
	assert.False(t, has)
}

// TestConsentNoOpWithoutBanner: the mainland deployment has no banner, and a
// page without one must navigate exactly as before even when a selector is
// configured.
func TestConsentNoOpWithoutBanner(t *testing.T) {
	humanize.SetProvider(fastTiming{})
	t.Cleanup(func() { humanize.SetProvider(humanize.DefaultProvider{}) })
	withConsentSite(t, "button.cookie-banner__btn--primary")

	b := browser.NewBrowser(true)
	defer b.Close()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><div id="app">ok</div></body></html>`)
	}))
	defer plain.Close()

	page := b.NewPage()
	defer func() { _ = page.Close() }()

	require.NoError(t, navigateFrom(context.Background(), page, plain.URL+"/", "", navWaitLoad))
}
