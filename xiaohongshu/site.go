package xiaohongshu

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// Site is the deployment profile: the handful of facts that differ between the
// mainland site (xiaohongshu.com) and the international one (rednote.com).
//
// It is a value struct with two presets rather than an interface with two
// implementations: roughly all of the behaviour is shared, and an interface
// would push every URL builder behind a method call for the sake of two rows
// of data. It is set once at startup and read everywhere after that.
type Site struct {
	// Name is the operator-facing identifier, also what XHS_SITE accepts and
	// what the session file records.
	Name string
	// Domain is the registrable domain, without a host label: the value a
	// cookie's domain ends with.
	Domain string
	// NotePath is the path shape of a single note, with one %s for the note id.
	// The deployments genuinely differ here, which is why a bare domain knob
	// would build URLs that 404.
	NotePath string
	// Timezone is the browser timezone this deployment should report, empty
	// meaning "follow the host". XHS_TIMEZONE overrides it either way.
	//
	// The mainland site keeps Asia/Shanghai (issue #2): a CN account on a CN
	// egress IP reporting a European zone is incoherent. The international
	// site reverses it for the same reason with the sign flipped — a
	// UK-registered account on a UK egress IP has no business claiming
	// Shanghai. Language stays zh-CN on both; the rednote UI is Chinese.
	Timezone string
	// ConsentAcceptSelector is the cookie banner's accept button, empty when
	// the deployment has no banner. See consent.go: on rednote the banner
	// comes with a full-viewport overlay that intercepts every click, and
	// consent is stored in the browser profile rather than in the cookie jar.
	ConsentAcceptSelector string
}

// The two known deployments.
var (
	SiteXiaohongshu = Site{
		Name:     "xiaohongshu",
		Domain:   "xiaohongshu.com",
		NotePath: "/explore/%s",
		Timezone: "Asia/Shanghai",
		// 大陆站没有 cookie 横幅
		ConsentAcceptSelector: "",
	}

	SiteRednote = Site{
		Name:     "rednote",
		Domain:   "rednote.com",
		NotePath: "/discovery/item/%s",
		Timezone: "", // follow the operator's host zone

		ConsentAcceptSelector: "button.cookie-banner__btn--primary",
	}
)

// activeSite is the deployment in force for this process. It is not guarded by
// a mutex on purpose: SetSite runs once at startup, before any browser work,
// and everything afterwards only reads it.
var activeSite = SiteXiaohongshu

func init() { SetSite(SiteXiaohongshu) }

// SetSite installs the deployment profile and refreshes everything derived
// from it. Call it once, at startup, before the browser is launched.
func SetSite(s Site) {
	activeSite = s

	// The fixed landing pages are package vars precisely so that every
	// existing use of them stays a plain identifier.
	urlHome = s.Home()
	urlExplore = s.Explore()
	urlNotification = s.Notification()
	urlOfPublic = s.CreatorPublish()
}

// ActiveSite returns the deployment profile in force.
func ActiveSite() Site { return activeSite }

// Home is the site root, no trailing slash.
func (s Site) Home() string { return "https://www." + s.Domain }

// Explore is the main feed, which doubles as the referrer for most deep links.
func (s Site) Explore() string { return s.Home() + "/explore" }

// Notification is the notification centre.
func (s Site) Notification() string { return s.Home() + "/notification" }

// CreatorHost is the creator centre's host, used to recognise the sidebar's
// 发布 link and to tell when a click has arrived there.
func (s Site) CreatorHost() string { return "creator." + s.Domain }

// CreatorPublish is the publish page on the creator centre.
func (s Site) CreatorPublish() string {
	return "https://" + s.CreatorHost() + "/publish/publish?source=official"
}

// NoteURL builds a note deep link. xsecSource must match the page the token
// came from, and must agree with the Referer sent alongside it.
func (s Site) NoteURL(feedID, xsecToken, xsecSource string) string {
	if xsecSource == "" {
		xsecSource = xsecSourceFeed
	}
	return s.Home() + fmt.Sprintf(s.NotePath, feedID) +
		fmt.Sprintf("?xsec_token=%s&xsec_source=%s", xsecToken, xsecSource)
}

// UserProfileURL builds a profile deep link, optionally on a non-default tab.
func (s Site) UserProfileURL(userID, xsecToken string, tab ProfileTab) string {
	u := fmt.Sprintf("%s/user/profile/%s?xsec_token=%s&xsec_source=%s",
		s.Home(), userID, xsecToken, xsecSourceNote)
	if tab != "" && tab != TabNotes {
		u += fmt.Sprintf("&tab=%s&subTab=note", tab)
	}
	return u
}

// SearchURL builds the search results page for a keyword.
func (s Site) SearchURL(keyword string) string {
	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("source", "web_explore_feed")

	return fmt.Sprintf("%s/search_result?%s", s.Home(), values.Encode())
}

// OnMainSite reports whether a URL is a page of the main site, which is where
// the sidebar lives.
func (s Site) OnMainSite(current string) bool {
	return strings.HasPrefix(current, s.Home()+"/")
}

// OnCreatorSite reports whether a URL belongs to the creator centre.
func (s Site) OnCreatorSite(current string) bool {
	return strings.Contains(current, s.CreatorHost())
}

// BrowserTimezone is the timezone to hand the browser when XHS_TIMEZONE says
// nothing. An empty result means "no opinion", which the browser layer turns
// into its own default rather than into the host zone.
func (s Site) BrowserTimezone() string {
	if s.Timezone != "" {
		return s.Timezone
	}
	return hostTimezone()
}

// localtimePath is the symlink consulted when the runtime will not name the
// host zone. A var so the test can point it somewhere harmless.
var localtimePath = "/etc/localtime"

// hostTimezone is the operator's own IANA zone name, or "" when it cannot be
// established at all.
//
// time.Local answers "Local" whenever TZ is unset, which is the normal state of
// a desktop or a plain container — Go only carries a zone name when TZ names
// one. Taking that as "unknown" made the rednote default unreachable in
// practice: the empty result fell through to the browser layer's own default,
// Asia/Shanghai, so an overseas account on an overseas exit IP reported
// Shanghai after all, which is the incoherence #18 set out to avoid. Reading
// the symlink recovers the name on both macOS and Linux.
func hostTimezone() string {
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	return zoneFromLocaltime(localtimePath)
}

// zoneFromLocaltime derives an IANA zone name from the /etc/localtime symlink,
// e.g. /var/db/timezone/zoneinfo/Europe/London -> Europe/London. It returns ""
// for a copied file rather than a link, for a destination outside a zoneinfo
// tree, and for any name the runtime cannot then load.
func zoneFromLocaltime(path string) string {
	dest, err := os.Readlink(path)
	if err != nil {
		return ""
	}

	const marker = "zoneinfo/"
	i := strings.LastIndex(dest, marker)
	if i < 0 {
		return ""
	}

	name := strings.Trim(dest[i+len(marker):], "/")
	if name == "" {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}

// SiteByName maps an operator-supplied name to a preset. Names only: a raw
// domain is rejected, because half the profile is not the domain.
func SiteByName(name string) (Site, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case SiteXiaohongshu.Name:
		return SiteXiaohongshu, true
	case SiteRednote.Name:
		return SiteRednote, true
	default:
		return Site{}, false
	}
}

// KnownSiteNames lists the accepted names, for error messages.
func KnownSiteNames() []string {
	return []string{SiteXiaohongshu.Name, SiteRednote.Name}
}

// jarCookie is the only field of a stored cookie this package cares about.
type jarCookie struct {
	Domain string `json:"domain"`
}

// sniffSiteFromJar guesses the deployment from the domains in a stored cookie
// jar. It reports a site only when the evidence is unambiguous: an empty jar,
// an unparsable one, one with no recognisable domain, or one holding cookies
// for both deployments all return false.
//
// This exists for jars written before the session file recorded a site — the
// only signal available to an operator upgrading in place, and the cost of
// guessing wrong is total failure plus a self-inflicted cooldown.
func sniffSiteFromJar(raw []byte) (Site, bool) {
	if len(raw) == 0 {
		return Site{}, false
	}

	var cks []jarCookie
	if err := json.Unmarshal(raw, &cks); err != nil {
		return Site{}, false
	}

	var seen []Site
	for _, preset := range []Site{SiteXiaohongshu, SiteRednote} {
		for _, c := range cks {
			if cookies.DomainMatches(c.Domain, preset.Domain) {
				seen = append(seen, preset)
				break
			}
		}
	}

	if len(seen) != 1 {
		return Site{}, false
	}
	return seen[0], true
}

// jarHasSessionFor reports whether a stored jar holds any cookie for the given
// site. A v1 jar (bare array) and a v2 payload both parse here, because the
// caller hands over the cookies array either way.
func jarHasSessionFor(raw []byte, s Site) bool {
	if len(raw) == 0 {
		return false
	}

	var cks []jarCookie
	if err := json.Unmarshal(raw, &cks); err != nil {
		// Unreadable but non-empty: assume a session was meant to be there
		// rather than silently deciding a login modal is normal.
		return true
	}

	for _, c := range cks {
		if cookies.DomainMatches(c.Domain, s.Domain) {
			return true
		}
	}
	return false
}
