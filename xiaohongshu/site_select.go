package xiaohongshu

import (
	"fmt"
	"os"
	"strings"

	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// siteEnvVar is the operator's override. It takes a preset name, never a raw
// domain: the domain is only half of what a deployment profile carries.
const siteEnvVar = "XHS_SITE"

// ResolveSite decides which deployment this process talks to, in the order
// explicit flag > XHS_SITE > the site recorded in the session file > the
// domains of the cookies in that file > xiaohongshu.
//
// It returns the site and a short reason, so startup can log the choice the
// way the browser manager logs its seeding decision. The caller is expected to
// SetSite the result once and never ask again: delete_cookies removes the
// session file, and re-resolving afterwards would silently change deployment
// mid-run.
//
// An error is fatal by design. Running with the wrong profile means every page
// shows a login modal, which trips the risk detector's two-signal rule and
// drops the account into a 30-minute cooldown — the failure is worse than not
// starting.
func ResolveSite(explicit string, store cookies.Cookier) (Site, string, error) {
	var jar []byte
	var fileSite string
	if store != nil {
		jar, _ = store.LoadCookies()
		fileSite = store.LoadSite()
	}

	return selectSite(explicit, os.Getenv(siteEnvVar), fileSite, jar)
}

// selectSite is ResolveSite without the I/O, which is what the tests drive.
func selectSite(flagName, envName, fileName string, jar []byte) (Site, string, error) {
	sniffed, sniffOK := sniffSiteFromJar(jar)

	// An explicit request — flag or environment — wins, and disagreeing with
	// the jar in hand is fatal rather than a warning.
	requested, source := flagName, "-site flag"
	if strings.TrimSpace(requested) == "" {
		requested, source = envName, siteEnvVar
	}
	if strings.TrimSpace(requested) != "" {
		site, ok := SiteByName(requested)
		if !ok {
			return Site{}, "", fmt.Errorf("unknown site %q from %s: expected one of %s (names, not domains)",
				requested, source, strings.Join(KnownSiteNames(), ", "))
		}
		if sniffOK && sniffed.Name != site.Name {
			return Site{}, "", siteMismatchError(source, site, sniffed)
		}
		return site, "requested by " + source, nil
	}

	// The session file's own record: written the last time cookies were
	// exported, so it is the answer for everyone who has logged in once.
	if strings.TrimSpace(fileName) != "" {
		site, ok := SiteByName(fileName)
		switch {
		case !ok:
			// Our own field, unreadable: fall through to the jar rather than
			// refuse to start over a value nobody typed.
		case sniffOK && sniffed.Name != site.Name:
			return Site{}, "", siteMismatchError("the session file's site field", site, sniffed)
		default:
			return site, "recorded in the session file", nil
		}
	}

	// A jar written before the site field existed: its cookie domains are the
	// only signal an operator upgrading in place has.
	if sniffOK {
		return sniffed, "sniffed from the cookie domains in the session file", nil
	}

	return SiteXiaohongshu, "default (no override, no session file to learn from)", nil
}

// siteMismatchError explains a disagreement by naming both sides. Whichever
// one is wrong, the operator has to know which two things failed to agree.
func siteMismatchError(source string, want, jar Site) error {
	return fmt.Errorf(
		"site mismatch: %s asks for %s (%s) but the saved session holds cookies for %s (%s); "+
			"log in again for %s, or point %s at %s",
		source, want.Name, want.Domain, jar.Name, jar.Domain,
		want.Name, siteEnvVar, jar.Name)
}
