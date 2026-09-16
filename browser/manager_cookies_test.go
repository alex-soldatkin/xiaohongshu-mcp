package browser

import (
	"encoding/json"
	"testing"

	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The session file is supposed to be one account's session. A probe pointed at
// xiaohongshu.com while seeded from a rednote jar wrote fifteen
// .xiaohongshu.com cookies into it, including a guest CN web_session
// (issue #21). These tests pin the filter that stops that, on both ends.

func TestKeepDomainSplitsTheJar(t *testing.T) {
	jar := []*proto.NetworkCookie{
		{Name: "web_session", Domain: ".rednote.com"},
		{Name: "acw_tc", Domain: "rednote.com"},
		{Name: "a1", Domain: "www.rednote.com"},
		{Name: "web_session", Domain: ".xiaohongshu.com"},
		{Name: "probe", Domain: "127.0.0.1"},
	}

	kept, dropped := keepDomain(jar, "rednote.com")
	require.Len(t, kept, 3)
	for _, c := range kept {
		assert.Contains(t, c.Domain, "rednote.com")
	}
	assert.Equal(t, []string{".xiaohongshu.com", "127.0.0.1"}, dropped)
}

// An unconfigured deployment keeps everything: a manager with no site is a test
// fixture, not an account.
func TestKeepDomainWithoutSiteKeepsEverything(t *testing.T) {
	jar := []*proto.NetworkCookie{{Name: "probe", Domain: "127.0.0.1"}}

	kept, dropped := keepDomain(jar, "")
	assert.Equal(t, jar, kept)
	assert.Empty(t, dropped)
}

func TestKeepDomainJSON(t *testing.T) {
	raw := `[{"name":"web_session","domain":".rednote.com","value":"real","httpOnly":true},
	         {"name":"web_session","domain":".xiaohongshu.com","value":"guest"}]`

	out, dropped := keepDomainJSON(raw, "rednote.com")
	assert.Equal(t, []string{".xiaohongshu.com"}, dropped)

	var jar []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &jar))
	require.Len(t, jar, 1)
	assert.Equal(t, "real", jar[0]["value"])
	// Fields ride through untouched rather than being rebuilt from a struct
	// this build happens to know about.
	assert.Equal(t, true, jar[0]["httpOnly"])
}

// Nothing to drop means nothing to rewrite: the stored bytes stay as they are.
func TestKeepDomainJSONLeavesACleanJarAlone(t *testing.T) {
	raw := `[{"name":"web_session","domain":".rednote.com"}]`

	out, dropped := keepDomainJSON(raw, "rednote.com")
	assert.Equal(t, raw, out)
	assert.Empty(t, dropped)
}

// Unreadable is not the same as foreign. A jar that will not parse is handed on
// untouched rather than thrown away.
func TestKeepDomainJSONPassesThroughWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", `{"version":2}`} {
		out, dropped := keepDomainJSON(raw, "rednote.com")
		assert.Equal(t, raw, out)
		assert.Empty(t, dropped)
	}
}

func TestUniqueSorted(t *testing.T) {
	in := []string{".xiaohongshu.com", "127.0.0.1", ".xiaohongshu.com", ""}
	assert.Equal(t, []string{"(none)", ".xiaohongshu.com", "127.0.0.1"}, uniqueSorted(in))
}
