package xiaohongshu

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
)

// Where a note opened from the notification centre really came from (issue #10).
//
// Every other surface tells us its xsec_source because we know what surface it
// is: the home feed hands out pc_feed, search results pc_search, profile
// listings pc_note. The notification list was the one gap — the notes it names
// are read back out of __INITIAL_STATE__, which carries the token but not the
// source, so those notes fell back to claiming they came from the feed.
//
// Guessing a value there would have been the exact failure this issue is about:
// an xsec_source the server can check against a Referer that says otherwise.
// So we do not guess. The notification page renders each note as an ordinary
// link, and that link's href is the site's own answer to the question. We read
// it and remember it. If a future layout stops rendering hrefs, nothing is
// recorded and the honest pc_feed default is what remains — no worse than
// before, and still not a guess.

// noteIDPattern is the shape of a note id in a URL path. Anything else in that
// position belongs to some other kind of link and is ignored.
var noteIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{16,32}$`)

// notePathPrefixes are the path shapes under which the site links to a note.
var notePathPrefixes = []string{"/explore/", "/discovery/item/", "/search_result/"}

// noteLink is one note link read off a page, with the provenance the site put
// in it.
type noteLink struct {
	id     string
	source string
}

// parseNoteLinks extracts the note links from a list of hrefs, keeping only
// those that actually name a note and actually carry an xsec_source. A link
// without a source teaches us nothing, and recording it would overwrite a
// better record with a blank.
func parseNoteLinks(hrefs []string) []noteLink {
	links := make([]noteLink, 0, len(hrefs))
	seen := make(map[string]struct{}, len(hrefs))

	for _, href := range hrefs {
		parsed, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			continue
		}

		id, ok := noteIDFromPath(parsed.Path)
		if !ok {
			continue
		}
		source := parsed.Query().Get("xsec_source")
		if source == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}

		seen[id] = struct{}{}
		links = append(links, noteLink{id: id, source: source})
	}
	return links
}

// noteIDFromPath returns the note id a path names, if it names one.
func noteIDFromPath(path string) (string, bool) {
	for _, prefix := range notePathPrefixes {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		id := strings.Trim(strings.TrimPrefix(path, prefix), "/")
		if strings.Contains(id, "/") || !noteIDPattern.MatchString(id) {
			return "", false
		}
		return id, true
	}
	return "", false
}

// rememberNotificationNoteLinks records the provenance of every note the
// notification page links to, and returns how many it learned.
//
// Reading hrefs is cheap and cannot fail the caller: a layout without links
// simply teaches us nothing.
func rememberNotificationNoteLinks(page *rod.Page) int {
	// Narrowed to links that carry a token, so this stays a handful of
	// round-trips rather than one per anchor on the page.
	elems, err := page.Elements(`a[href*="xsec_token"]`)
	if err != nil {
		logrus.Debugf("读取通知页笔记链接失败，笔记来源沿用默认值: %v", err)
		return 0
	}

	hrefs := make([]string, 0, len(elems))
	for _, elem := range elems {
		href, err := elem.Attribute("href")
		if err != nil || href == nil {
			continue
		}
		hrefs = append(hrefs, *href)
	}

	links := parseNoteLinks(hrefs)
	for _, link := range links {
		noteSources.remember(link.id, link.source, urlNotification)
	}
	if len(links) > 0 {
		logrus.Debugf("从通知页记下 %d 条笔记的来源（xsec_source 取自站点自己的链接）", len(links))
	}
	return len(links)
}
