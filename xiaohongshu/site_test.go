package xiaohongshu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSite installs a deployment profile for one test and puts the default
// back afterwards, so a failure here cannot leak into the next test.
func withSite(t *testing.T, s Site) {
	t.Helper()
	SetSite(s)
	t.Cleanup(func() { SetSite(SiteXiaohongshu) })
}

// TestSitePresetURLs pins every builder for both deployments. The note path is
// the reason this profile is a struct and not a domain constant.
func TestSitePresetURLs(t *testing.T) {
	cn := SiteXiaohongshu
	assert.Equal(t, "https://www.xiaohongshu.com", cn.Home())
	assert.Equal(t, "https://www.xiaohongshu.com/explore", cn.Explore())
	assert.Equal(t, "https://www.xiaohongshu.com/notification", cn.Notification())
	assert.Equal(t, "creator.xiaohongshu.com", cn.CreatorHost())
	assert.Equal(t, "https://creator.xiaohongshu.com/publish/publish?source=official", cn.CreatorPublish())
	assert.Equal(t, "https://www.xiaohongshu.com/explore/abc?xsec_token=tok&xsec_source=pc_feed",
		cn.NoteURL("abc", "tok", "pc_feed"))
	assert.Equal(t, "https://www.xiaohongshu.com/user/profile/uid?xsec_token=tok&xsec_source=pc_note",
		cn.UserProfileURL("uid", "tok", TabNotes))
	assert.Equal(t, "https://www.xiaohongshu.com/search_result?keyword=%E7%8E%8B&source=web_explore_feed",
		cn.SearchURL("王"))

	rn := SiteRednote
	assert.Equal(t, "https://www.rednote.com", rn.Home())
	assert.Equal(t, "https://www.rednote.com/explore", rn.Explore())
	assert.Equal(t, "https://www.rednote.com/notification", rn.Notification())
	assert.Equal(t, "creator.rednote.com", rn.CreatorHost())
	assert.Equal(t, "https://creator.rednote.com/publish/publish?source=official", rn.CreatorPublish())
	assert.Equal(t, "https://www.rednote.com/discovery/item/abc?xsec_token=tok&xsec_source=pc_feed",
		rn.NoteURL("abc", "tok", "pc_feed"))
	assert.Equal(t, "https://www.rednote.com/user/profile/uid?xsec_token=tok&xsec_source=pc_note",
		rn.UserProfileURL("uid", "tok", TabNotes))
	assert.Equal(t, "https://www.rednote.com/search_result?keyword=%E7%8E%8B&source=web_explore_feed",
		rn.SearchURL("王"))
}

// TestSiteNoteURLDefaultSource keeps the old contract: an empty xsec_source
// still means the feed.
func TestSiteNoteURLDefaultSource(t *testing.T) {
	assert.Contains(t, SiteRednote.NoteURL("abc", "tok", ""), "xsec_source=pc_feed")
}

// TestSiteProfileTab checks the non-default tabs keep their query shape.
func TestSiteProfileTab(t *testing.T) {
	assert.Equal(t,
		"https://www.rednote.com/user/profile/uid?xsec_token=tok&xsec_source=pc_note&tab=fav&subTab=note",
		SiteRednote.UserProfileURL("uid", "tok", TabFavorites))
}

// TestSitePredicates covers the host tests that decide whether the sidebar is
// available and whether a click landed on the creator centre.
func TestSitePredicates(t *testing.T) {
	assert.True(t, SiteXiaohongshu.OnMainSite("https://www.xiaohongshu.com/explore"))
	assert.False(t, SiteXiaohongshu.OnMainSite("https://www.rednote.com/explore"))
	assert.False(t, SiteXiaohongshu.OnMainSite("https://creator.xiaohongshu.com/publish/publish"))
	// The bare root has no trailing slash and is not a page with a sidebar.
	assert.False(t, SiteXiaohongshu.OnMainSite("https://www.xiaohongshu.com"))

	assert.True(t, SiteRednote.OnMainSite("https://www.rednote.com/notification"))
	assert.True(t, SiteRednote.OnCreatorSite("https://creator.rednote.com/publish/publish"))
	assert.False(t, SiteRednote.OnCreatorSite("https://creator.xiaohongshu.com/publish/publish"))
}

// TestSetSiteRewiresLandingPages proves the package vars follow the profile,
// which is what leaves every use of them untouched.
func TestSetSiteRewiresLandingPages(t *testing.T) {
	withSite(t, SiteRednote)

	assert.Equal(t, "https://www.rednote.com", urlHome)
	assert.Equal(t, "https://www.rednote.com/explore", urlExplore)
	assert.Equal(t, "https://www.rednote.com/notification", urlNotification)
	assert.Equal(t, "https://creator.rednote.com/publish/publish?source=official", urlOfPublic)

	// Everything derived from them follows: the note-source fallback referrer,
	// the sidebar predicate and the builders.
	_, referrer := feedEntryPoint(context.Background(), "never-seen")
	assert.Equal(t, "https://www.rednote.com/explore", referrer)
	assert.True(t, onMainSite("https://www.rednote.com/explore"))
	assert.False(t, onMainSite("https://www.xiaohongshu.com/explore"))
	assert.Equal(t, "https://www.rednote.com/discovery/item/n1?xsec_token=t&xsec_source=pc_feed",
		makeFeedDetailURL("n1", "t", ""))
	assert.Contains(t, makeSearchURL("x"), "https://www.rednote.com/search_result?")
	assert.Contains(t, makeUserProfileURL("u", "t", TabNotes), "https://www.rednote.com/user/profile/u")
}

func TestSiteByName(t *testing.T) {
	for name, want := range map[string]Site{
		"xiaohongshu": SiteXiaohongshu,
		"rednote":     SiteRednote,
		" REDnote ":   SiteRednote,
	} {
		got, ok := SiteByName(name)
		require.True(t, ok, name)
		assert.Equal(t, want, got)
	}

	// Domains are not names: half the profile is not the domain.
	for _, bad := range []string{"", "rednote.com", "www.rednote.com", "xhs"} {
		_, ok := SiteByName(bad)
		assert.False(t, ok, bad)
	}
}

// jarJSON builds a cookies array in the shape the session file stores.
func jarJSON(t *testing.T, domains ...string) []byte {
	t.Helper()
	type ck struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
	}
	var cks []ck
	for i, d := range domains {
		cks = append(cks, ck{Name: "c" + string(rune('a'+i)), Domain: d})
	}
	data, err := json.Marshal(cks)
	require.NoError(t, err)
	return data
}

func TestSniffSiteFromJar(t *testing.T) {
	got, ok := sniffSiteFromJar(jarJSON(t, "www.rednote.com", ".rednote.com", "as.rednote.com"))
	require.True(t, ok)
	assert.Equal(t, SiteRednote, got)

	got, ok = sniffSiteFromJar(jarJSON(t, ".xiaohongshu.com", "creator.xiaohongshu.com"))
	require.True(t, ok)
	assert.Equal(t, SiteXiaohongshu, got)

	// No evidence, or contradictory evidence, is not a guess.
	for _, jar := range [][]byte{
		nil,
		[]byte("[]"),
		[]byte("not json"),
		jarJSON(t, "example.com"),
		jarJSON(t, ".rednote.com", ".xiaohongshu.com"),
	} {
		_, ok := sniffSiteFromJar(jar)
		assert.False(t, ok, string(jar))
	}
}

// TestSniffSiteFromRealJar runs the sniff over the repository's own session
// file when one is present. It is read-only, and it is the only test with a
// real-world fixture behind it.
func TestSniffSiteFromRealJar(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "cookies.json"))
	if err != nil {
		t.Skip("no session file in the repository root")
	}

	var f struct {
		Cookies json.RawMessage `json:"cookies"`
	}
	require.NoError(t, json.Unmarshal(data, &f))

	site, ok := sniffSiteFromJar(f.Cookies)
	require.True(t, ok, "the saved jar names no known deployment")
	assert.Contains(t, KnownSiteNames(), site.Name)
}

func TestJarHasSessionFor(t *testing.T) {
	rednote := jarJSON(t, "www.rednote.com", ".rednote.com")

	assert.True(t, jarHasSessionFor(rednote, SiteRednote))
	// The crux of C8: a jar for the other deployment is no session here.
	assert.False(t, jarHasSessionFor(rednote, SiteXiaohongshu))

	assert.False(t, jarHasSessionFor(nil, SiteRednote))
	assert.False(t, jarHasSessionFor([]byte("[]"), SiteRednote))
	// Non-empty but unreadable: assume a session was meant to be there rather
	// than deciding a login modal is normal.
	assert.True(t, jarHasSessionFor([]byte("garbage"), SiteRednote))
}

func TestSiteBrowserTimezone(t *testing.T) {
	// The CN preset pins Shanghai (issue #2); the international one follows
	// the operator's host zone, which is exactly what issue #2 forbids for a
	// CN account and requires for an overseas one.
	assert.Equal(t, "Asia/Shanghai", SiteXiaohongshu.BrowserTimezone())
	assert.Equal(t, "", SiteRednote.Timezone)
	assert.Equal(t, hostTimezone(), SiteRednote.BrowserTimezone())
}

// TestZoneFromLocaltime covers the fallback that makes the rednote default
// reachable at all. time.Local is named "Local" whenever TZ is unset, which is
// the ordinary state of a desktop and of most containers, so without this the
// overseas preset silently fell back to the browser's Asia/Shanghai default.
func TestZoneFromLocaltime(t *testing.T) {
	dir := t.TempDir()

	link := filepath.Join(dir, "localtime")
	require.NoError(t, os.Symlink("/var/db/timezone/zoneinfo/Europe/London", link))
	assert.Equal(t, "Europe/London", zoneFromLocaltime(link))

	// The Linux layout, and a trailing slash.
	linux := filepath.Join(dir, "linux")
	require.NoError(t, os.Symlink("/usr/share/zoneinfo/Asia/Tokyo", linux))
	assert.Equal(t, "Asia/Tokyo", zoneFromLocaltime(linux))

	// A destination outside any zoneinfo tree, a name no zone database knows,
	// a plain file instead of a link, and a missing path all mean "unknown"
	// rather than a guess.
	elsewhere := filepath.Join(dir, "elsewhere")
	require.NoError(t, os.Symlink("/etc/something", elsewhere))
	assert.Equal(t, "", zoneFromLocaltime(elsewhere))

	bogus := filepath.Join(dir, "bogus")
	require.NoError(t, os.Symlink("/usr/share/zoneinfo/Nowhere/Nothing", bogus))
	assert.Equal(t, "", zoneFromLocaltime(bogus))

	plain := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(plain, []byte("not a link"), 0o600))
	assert.Equal(t, "", zoneFromLocaltime(plain))

	assert.Equal(t, "", zoneFromLocaltime(filepath.Join(dir, "missing")))
}
