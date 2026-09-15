//go:build integration

// Integration test: starts a real browser. Not compiled by a plain go test.
// Run: go test -tags integration ./xiaohongshu/ -run TestRiskControl
package xiaohongshu

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

// TestRiskControlScreenshotOnRealPage exercises the two CDP calls the detector
// makes on a live page — Page.getNavigationHistory via Info, and the screenshot
// — because neither had been run on the bundled build before this landed. It
// also proves the fake switch produces the typed error, which is the only way
// to exercise the path until a real challenge is seen.
func TestRiskControlScreenshotOnRealPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><div id="app">hello</div></body></html>`))
	}))
	defer srv.Close()

	b := browser.NewBrowser(true)
	defer b.Close()

	page := b.NewPage()
	defer page.Close()

	require.NoError(t, page.Navigate(srv.URL))
	require.NoError(t, page.WaitLoad())

	// A plain page must not be reported as a challenge.
	d := &riskDetector{
		window:          riskSignalWindow,
		threshold:       riskSignalThreshold,
		now:             newRiskDetector().now,
		screenshot:      saveRiskScreenshot,
		sessionExpected: func() bool { return true },
		headful:         func() bool { return false },
	}
	require.NoError(t, d.check(page))

	// The screenshot must land on disk: it is the evidence that turns the
	// guessed markers into confirmed ones.
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir) // configs.GetImagesPath lives under os.TempDir

	t.Setenv("XHS_FAKE_RISK_CONTROL", "1")
	err := d.check(page)
	require.Error(t, err)

	rc, ok := myerrors.AsRiskControl(err)
	require.True(t, ok, "want *ErrRiskControl, got %T", err)
	require.Equal(t, riskKindFake, rc.Kind)
	require.True(t, strings.HasPrefix(rc.URL, srv.URL), "URL = %s", rc.URL)
	require.NotEmpty(t, rc.Screenshot, "screenshot path empty — Page.captureScreenshot failed")

	info, statErr := os.Stat(rc.Screenshot)
	require.NoError(t, statErr)
	require.Greater(t, info.Size(), int64(0))
	require.Equal(t, ".png", filepath.Ext(rc.Screenshot))
}
