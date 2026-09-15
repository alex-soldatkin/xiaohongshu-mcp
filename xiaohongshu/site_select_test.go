package xiaohongshu

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// TestSelectSitePrecedence walks the documented order: flag, then XHS_SITE,
// then the session file's own field, then the cookie domains, then the
// default.
func TestSelectSitePrecedence(t *testing.T) {
	cnJar := jarJSON(t, ".xiaohongshu.com")
	rnJar := jarJSON(t, ".rednote.com")

	tests := []struct {
		name       string
		flag, env  string
		fileSite   string
		jar        []byte
		want       Site
		wantReason string
	}{
		{
			name: "flag beats everything", flag: "rednote", env: "xiaohongshu",
			fileSite: "xiaohongshu", jar: rnJar,
			want: SiteRednote, wantReason: "requested by -site flag",
		},
		{
			name: "env beats the file", env: "rednote", fileSite: "rednote", jar: rnJar,
			want: SiteRednote, wantReason: "requested by XHS_SITE",
		},
		{
			name: "file beats the sniff", fileSite: "rednote", jar: rnJar,
			want: SiteRednote, wantReason: "recorded in the session file",
		},
		{
			name: "sniff when the file predates the field", jar: rnJar,
			want: SiteRednote, wantReason: "sniffed from the cookie domains in the session file",
		},
		{
			name: "sniff recognises the mainland jar too", jar: cnJar,
			want: SiteXiaohongshu, wantReason: "sniffed from the cookie domains in the session file",
		},
		{
			name: "default on a fresh install",
			want: SiteXiaohongshu, wantReason: "default (no override, no session file to learn from)",
		},
		{
			name:     "an unreadable file field falls through to the sniff",
			fileSite: "xhs", jar: rnJar,
			want: SiteRednote, wantReason: "sniffed from the cookie domains in the session file",
		},
		{
			name: "an empty jar is no evidence", env: "rednote",
			want: SiteRednote, wantReason: "requested by XHS_SITE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason, err := selectSite(tt.flag, tt.env, tt.fileSite, tt.jar)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

// TestSelectSiteMismatchIsFatal is the decision from the plan: an explicit
// request that disagrees with the saved cookies stops startup, naming both.
// Carrying on means a login modal on every page, which trips the risk
// detector's two-signal rule and drops the account into a cooldown.
func TestSelectSiteMismatchIsFatal(t *testing.T) {
	rnJar := jarJSON(t, ".rednote.com", "www.rednote.com")

	_, _, err := selectSite("", "xiaohongshu", "", rnJar)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xiaohongshu")
	assert.Contains(t, err.Error(), "rednote.com")
	assert.Contains(t, err.Error(), "XHS_SITE")

	_, _, err = selectSite("xiaohongshu", "", "", rnJar)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-site flag")

	// The file's own field is held to the same standard: a jar swapped under a
	// label is the same unworkable state.
	_, _, err = selectSite("", "", "xiaohongshu", rnJar)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session file")

	// Agreement is not a mismatch.
	_, _, err = selectSite("", "rednote", "", rnJar)
	assert.NoError(t, err)
}

// TestSelectSiteRejectsUnknownNames keeps XHS_SITE a name, not a domain.
func TestSelectSiteRejectsUnknownNames(t *testing.T) {
	for _, bad := range []string{"rednote.com", "https://www.rednote.com", "xhs"} {
		_, _, err := selectSite("", bad, "", nil)
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "expected one of")
	}
}

// TestResolveSiteReadsTheSessionFile checks the I/O wrapper end to end,
// against a real file written by the cookies package.
func TestResolveSiteReadsTheSessionFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	store := cookies.NewLoadCookie(path)
	require.NoError(t, store.SaveCookies(jarJSON(t, ".rednote.com")))

	t.Setenv(siteEnvVar, "")
	site, reason, err := ResolveSite("", store)
	require.NoError(t, err)
	assert.Equal(t, SiteRednote, site)
	assert.Contains(t, reason, "sniffed")

	// Once recorded, the field answers without a sniff.
	require.NoError(t, store.SaveSite(SiteRednote.Name))
	site, reason, err = ResolveSite("", store)
	require.NoError(t, err)
	assert.Equal(t, SiteRednote, site)
	assert.Equal(t, "recorded in the session file", reason)

	// And the environment still wins over both.
	t.Setenv(siteEnvVar, "rednote")
	_, reason, err = ResolveSite("", store)
	require.NoError(t, err)
	assert.Equal(t, "requested by "+siteEnvVar, reason)
}

// TestCookieSessionExpectedIsDomainAware is C8: with a rednote jar and the
// mainland profile in force, a login modal is expected, not a risk signal.
func TestCookieSessionExpectedIsDomainAware(t *testing.T) {
	writeJar := func(t *testing.T, domains ...string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cookies.json")
		data, err := json.Marshal(map[string]any{
			"version": 2,
			"cookies": json.RawMessage(jarJSON(t, domains...)),
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		t.Setenv("COOKIES_PATH", path)
	}

	t.Run("a rednote jar is a session on rednote", func(t *testing.T) {
		writeJar(t, "www.rednote.com", ".rednote.com")
		withSite(t, SiteRednote)
		assert.True(t, cookieSessionExpected())
	})

	t.Run("a rednote jar is no session on the mainland site", func(t *testing.T) {
		writeJar(t, "www.rednote.com", ".rednote.com")
		withSite(t, SiteXiaohongshu)
		assert.False(t, cookieSessionExpected())
	})

	t.Run("a mainland jar is a session on the mainland site", func(t *testing.T) {
		writeJar(t, ".xiaohongshu.com")
		withSite(t, SiteXiaohongshu)
		assert.True(t, cookieSessionExpected())
	})

	t.Run("no file at all", func(t *testing.T) {
		t.Setenv("COOKIES_PATH", filepath.Join(t.TempDir(), "absent.json"))
		withSite(t, SiteRednote)
		assert.False(t, cookieSessionExpected())
	})
}
