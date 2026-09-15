//go:build integration

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/pacing"
)

// TestServiceReusesOneBrowser is the service-level half of issue #6's
// verification: two consecutive actions must share one browser process, and the
// cookie backup must keep up with the live jar.
//
// The browser is counted through the launch log line rather than through the
// manager's internals, because that is the observation an operator can make on
// a running server.
//
//	GOARCH=arm64 go test -tags integration -run TestServiceReusesOneBrowser -v .
func TestServiceReusesOneBrowser(t *testing.T) {
	if _, err := browser.EnsureBrowser(); err != nil {
		t.Skipf("SKIP: bundled browser unavailable (set GOARCH=arm64 on an arm64 host): %v", err)
	}

	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "cookies.json")
	t.Setenv("COOKIES_PATH", sessionPath)
	t.Setenv("XHS_PROFILE_DIR", filepath.Join(dir, "profile"))
	t.Setenv("XHS_PACING_STATE", filepath.Join(dir, "pacing_state.json"))
	t.Setenv("XHS_MIN_GAP_MS", "0") // no point paying the human pause in a test

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "probe_session", Value: "v", Path: "/", MaxAge: 3600})
		_, _ = w.Write([]byte("<!doctype html><title>t</title><p>t</p>"))
	}))
	defer srv.Close()

	var logged bytes.Buffer
	originalOut := logrus.StandardLogger().Out
	logrus.SetOutput(&logged)
	defer logrus.SetOutput(originalOut)

	configs.InitHeadless(true)
	configs.SetFingerprintSeed(98759)
	svc := NewXiaohongshuService()
	defer svc.Close(context.Background())

	visit := func() {
		require.NoError(t, svc.run(context.Background(), pacing.ClassRead, func(page *rod.Page) error {
			page.Timeout(30 * time.Second).MustNavigate(srv.URL).MustWaitLoad()
			return nil
		}))
	}

	visit()
	session := cookies.NewLoadCookie(sessionPath)
	firstSaved := session.LoadSavedAt()
	require.False(t, firstSaved.IsZero(), "the first action did not export the cookie jar")

	// RFC3339 has second precision, so a second action inside the same second
	// would be indistinguishable from no export at all.
	time.Sleep(1100 * time.Millisecond)
	visit()

	launches := strings.Count(logged.String(), "fingerprint enabled")
	assert.Equal(t, 1, launches, "expected one browser launch across two actions, saw %d:\n%s", launches, logged.String())

	assert.True(t, session.LoadSavedAt().After(firstSaved),
		"cookies.json saved_at did not advance after the second action")
}
