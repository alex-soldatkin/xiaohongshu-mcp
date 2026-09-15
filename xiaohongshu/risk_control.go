package xiaohongshu

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/html"

	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

// Risk-control detection (issue #11).
//
// When Xiaohongshu challenges a session — slider captcha, security
// interstitial, forced re-login — nothing in the page matches the selectors the
// callers are waiting for, so every action degrades into a generic timeout. A
// timeout is indistinguishable from "the markup changed" or "the network was
// slow", which means the one signal that actually matters, *this account has
// been flagged*, is the one the system cannot report.
//
// checkRiskControl runs on every navigation and before every submit, reports a
// typed *errors.ErrRiskControl, and — only once two signals have been seen —
// puts the pacing gate into a cooldown.
//
// Solving the challenge is explicitly out of scope. In headful mode the page is
// left open for a human; in headless mode we back off. Failing a slider
// repeatedly is worse for the account than not attempting it.

// Risk kinds. These are the Kind values carried by *errors.ErrRiskControl.
const (
	riskKindCaptchaURL   = "captcha_url"
	riskKindCaptchaDOM   = "captcha_dom"
	riskKindLoginRequire = "login_required"
	riskKindFake         = "fake"
)

// riskMarker is one row of the marker table.
//
// Everything below with confirmed=false is a GUESS. No real challenge has been
// observed on this account yet, so these were derived from the issue plan and
// from how comparable Chinese sites build their slider widgets — not from a
// captured page. The first time a real challenge is hit, dump the page HTML and
// the URL, and correct this table from that evidence; that is the only thing
// that turns a guess into a fact here.
type riskMarker struct {
	// kind is the risk kind reported when this marker matches.
	kind string
	// needle is matched case-insensitively. For urlMarkers it is matched
	// against the URL path only (never the query string, so a search for the
	// word "verify" cannot trip it). For DOM markers it is matched against
	// class/id attribute values or against visible text.
	needle string
	// confirmed is true only when the marker has been seen in a real page from
	// this site. Everything else is a hypothesis.
	confirmed bool
	// why records where the marker came from, so a future reader can tell
	// evidence from speculation without digging through git history.
	why string
}

// urlMarkers match against the URL path. A challenge usually redirects, so this
// is the strongest and least ambiguous of the three detectors.
var urlMarkers = []riskMarker{
	{riskKindCaptchaURL, "/website-login/captcha", false, "from the issue plan; not yet observed"},
	{riskKindCaptchaURL, "/web-login/captcha", false, "from the issue plan; not yet observed"},
	{riskKindCaptchaURL, "captcha", false, "generic: any path segment naming a captcha"},
	{riskKindCaptchaURL, "verify", false, "generic: security-verification interstitials; path only, never the query"},
}

// containerMarkers match the class or id attribute of any element. A captcha
// widget names itself in its container, which is why this is a strong signal
// even without matching text.
var containerMarkers = []riskMarker{
	{riskKindCaptchaDOM, "captcha", false, "covers the plan's guesses .red-captcha* and #red-captcha-container"},
	{riskKindCaptchaDOM, "slide-verify", false, "guess: common naming for slider widgets on Chinese sites"},
	{riskKindCaptchaDOM, "verify-code", false, "guess: SMS/graphic verification step"},
}

// textMarkers match visible text, but ONLY inside an overlay-ish ancestor (see
// overlayFragments). Unscoped, these would fire on any note whose author merely
// wrote the words 安全验证 — a false positive on a healthy account, on a page we
// were reading successfully.
var textMarkers = []riskMarker{
	{riskKindCaptchaDOM, "安全验证", false, "from the issue plan; not yet observed"},
	{riskKindCaptchaDOM, "请完成验证", false, "from the issue plan; not yet observed"},
	{riskKindCaptchaDOM, "拖动滑块", false, "from the issue plan; not yet observed"},
	{riskKindCaptchaDOM, "滑动验证", false, "guess: the other common phrasing of the same widget"},
}

// loginMarkers match the login modal. They are only consulted when a cookie
// session was expected: outside that case the modal is the normal state of a
// logged-out browser, not a risk-control event.
//
// Unlike everything else here, "login-container" is confirmed: login.go has
// been reading .login-container .qrcode-img in production since before this
// file existed.
var loginMarkers = []riskMarker{
	{riskKindLoginRequire, "login-container", true, "used by login.go:FetchQrcodeImage against the live site"},
}

// overlayFragments are class/id fragments that mark an element as a modal,
// mask or popup. Text markers only count inside one of these.
var overlayFragments = []string{"captcha", "verify", "mask", "modal", "dialog", "popup", "overlay"}

// riskSignal is one marker hit.
type riskSignal struct {
	kind   string
	marker string
	where  string // "url", "dom-container", "dom-text", "login", "fake"
}

func (s riskSignal) String() string {
	return fmt.Sprintf("%s(%s=%q)", s.kind, s.where, s.marker)
}

// detectRiskSignals is the whole detection logic, as a pure function over a
// page snapshot, so it can be tested against saved HTML fixtures without a
// browser.
//
// sessionExpected says whether cookies were loaded for this run. When false, a
// login modal is simply "not logged in" and is not reported.
func detectRiskSignals(pageURL, pageHTML string, sessionExpected bool) []riskSignal {
	var signals []riskSignal

	signals = append(signals, detectURLSignals(pageURL)...)
	signals = append(signals, detectDOMSignals(pageHTML, sessionExpected)...)

	return signals
}

// detectURLSignals matches the URL path only. Matching the query string would
// make a search for the word "verify" look like a security challenge.
func detectURLSignals(pageURL string) []riskSignal {
	if pageURL == "" {
		return nil
	}

	path := pageURL
	if u, err := url.Parse(pageURL); err == nil && u.Path != "" {
		path = u.Path
	} else if err == nil {
		// A URL with no path at all cannot match any of our markers.
		return nil
	}
	path = strings.ToLower(path)

	var signals []riskSignal
	for _, m := range urlMarkers {
		if strings.Contains(path, m.needle) {
			signals = append(signals, riskSignal{kind: m.kind, marker: m.needle, where: "url"})
			// One URL is one piece of evidence: the specific and the generic
			// marker matching the same path is not two independent signals.
			break
		}
	}
	return signals
}

// detectDOMSignals parses the document and looks for captcha containers,
// captcha text inside an overlay, and (when a session was expected) the login
// modal.
func detectDOMSignals(pageHTML string, sessionExpected bool) []riskSignal {
	if strings.TrimSpace(pageHTML) == "" {
		return nil
	}

	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		// A page we cannot parse is a page we cannot judge. Reporting a risk
		// event here would be a guess about a guess.
		logrus.Debugf("risk control: parsing page HTML failed, skipping DOM detectors: %v", err)
		return nil
	}

	seen := map[string]bool{}
	var signals []riskSignal
	add := func(s riskSignal) {
		key := s.where + "|" + s.marker
		if seen[key] {
			return
		}
		seen[key] = true
		signals = append(signals, s)
	}

	var walk func(n *html.Node, inOverlay bool)
	walk = func(n *html.Node, inOverlay bool) {
		if n.Type == html.ElementNode {
			// Script and style carry the whole site's vocabulary, including
			// every string the SPA might ever render. Matching text in there
			// would report a challenge on every page.
			switch n.Data {
			case "script", "style", "noscript", "template":
				return
			}

			ident := elementIdentity(n)
			for _, m := range containerMarkers {
				if strings.Contains(ident, m.needle) {
					add(riskSignal{kind: m.kind, marker: m.needle, where: "dom-container"})
				}
			}
			if sessionExpected {
				for _, m := range loginMarkers {
					if strings.Contains(ident, m.needle) {
						add(riskSignal{kind: m.kind, marker: m.needle, where: "login"})
					}
				}
			}
			if !inOverlay {
				inOverlay = n.Data == "dialog" || containsAny(ident, overlayFragments)
			}
		}

		if n.Type == html.TextNode && inOverlay {
			text := n.Data
			for _, m := range textMarkers {
				if strings.Contains(text, m.needle) {
					add(riskSignal{kind: m.kind, marker: m.needle, where: "dom-text"})
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inOverlay)
		}
	}
	walk(doc, false)

	return signals
}

// elementIdentity returns the lowercased class and id attributes joined, which
// is what the class/id fragment markers are matched against.
func elementIdentity(n *html.Node) string {
	var b strings.Builder
	for _, a := range n.Attr {
		switch a.Key {
		case "class", "id":
			b.WriteString(strings.ToLower(a.Val))
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// riskDetector holds the cross-call state: how many signals have been seen
// recently, and where to send a cooldown when the threshold is crossed.
type riskDetector struct {
	mu sync.Mutex
	// seen holds the timestamps of recent signals, pruned to window.
	seen []time.Time

	// window is how long a signal stays relevant, and threshold how many are
	// needed before a cooldown is entered.
	window    time.Duration
	threshold int

	// cooldown is the pacing gate's Cooldown, injected from service.go so this
	// package does not have to know about the gate. A zero duration means "use
	// the gate's configured default", which is XHS_RISK_COOLDOWN (30m) — the
	// gate already reads that env var, so there is no second knob here.
	cooldown func(time.Duration)

	now func() time.Time
	// screenshot saves an image of the challenge for the operator and returns
	// its path. Injected so tests do not write files.
	screenshot func(page *rod.Page, kind string) string
	// sessionExpected reports whether a cookie session was loaded.
	sessionExpected func() bool
	// headful reports whether a human could actually see the open window.
	headful func() bool
}

// riskSignalWindow / riskSignalThreshold implement the "two signals before a
// cooldown" rule from the plan. A false positive costs a healthy account 30
// minutes of silence, so a lone signal is reported to the caller and logged at
// warn but does not stop the world; a second one within the window does.
const (
	riskSignalWindow    = 15 * time.Minute
	riskSignalThreshold = 2
)

// riskCheckTimeout bounds the two CDP reads the check performs. The check must
// never be the reason a call hangs.
const riskCheckTimeout = 5 * time.Second

var defaultRiskDetector = newRiskDetector()

func newRiskDetector() *riskDetector {
	return &riskDetector{
		window:          riskSignalWindow,
		threshold:       riskSignalThreshold,
		now:             time.Now,
		screenshot:      saveRiskScreenshot,
		sessionExpected: cookieSessionExpected,
		headful:         func() bool { return !configs.IsHeadless() },
	}
}

// SetRiskCooldownHook installs the function that puts the pacing gate into a
// cooldown when risk control is detected twice. service.go wires this to
// Gate.Cooldown at construction; passing nil disables the cooldown side effect
// (detection still reports the typed error).
func SetRiskCooldownHook(f func(time.Duration)) {
	defaultRiskDetector.mu.Lock()
	defer defaultRiskDetector.mu.Unlock()
	defaultRiskDetector.cooldown = f
}

// checkRiskControl inspects the current page for a risk-control challenge.
//
// It returns nil when the page looks normal, and *errors.ErrRiskControl when it
// does not. It is deliberately forgiving about its own failures: if the URL or
// the HTML cannot be read, it returns nil rather than inventing a risk event.
func checkRiskControl(page *rod.Page) error {
	return defaultRiskDetector.check(page)
}

func (d *riskDetector) check(page *rod.Page) error {
	pageURL, signals := d.collect(page)
	if len(signals) == 0 {
		return nil
	}
	return d.report(page, pageURL, signals)
}

// collect gathers the signals for the current page.
func (d *riskDetector) collect(page *rod.Page) (string, []riskSignal) {
	// XHS_FAKE_RISK_CONTROL exists because no real challenge has been observed
	// yet: it exercises the whole path — typed error, screenshot, two-signal
	// rule, cooldown, HTTP 423 — without waiting for the site to flag us.
	if os.Getenv("XHS_FAKE_RISK_CONTROL") == "1" {
		fakeURL := "fake://risk-control"
		if page != nil {
			fakeURL = currentURL(page)
		}
		return fakeURL, []riskSignal{{kind: riskKindFake, marker: "XHS_FAKE_RISK_CONTROL=1", where: "fake"}}
	}

	if page == nil {
		return "", nil
	}

	pp := page.Timeout(riskCheckTimeout)

	info, err := pp.Info()
	if err != nil || info == nil {
		logrus.Debugf("risk control: cannot read page info, skipping check: %v", err)
		return "", nil
	}

	pageHTML, err := pp.HTML()
	if err != nil {
		logrus.Debugf("risk control: cannot read page HTML, URL detectors only: %v", err)
	}

	return info.URL, detectRiskSignals(info.URL, pageHTML, d.sessionExpectedSafe())
}

func (d *riskDetector) sessionExpectedSafe() bool {
	d.mu.Lock()
	f := d.sessionExpected
	d.mu.Unlock()
	if f == nil {
		return false
	}
	return f()
}

// report logs, screenshots, applies the two-signal rule and builds the typed
// error.
func (d *riskDetector) report(page *rod.Page, pageURL string, signals []riskSignal) error {
	// Warn, not error: until a real challenge has confirmed the marker table,
	// every one of these could be the table being wrong rather than the account
	// being flagged. Say so loudly, but do not claim certainty.
	logrus.Warnf("risk control suspected at %s: %v", pageURL, signals)

	kind := signals[0].kind
	shot := ""
	if f := d.screenshotFunc(); f != nil {
		shot = f(page, kind)
	}

	cooled := d.recordAndMaybeCooldown(len(signals))

	detail := d.detail(cooled, signals)

	return &myerrors.ErrRiskControl{
		Kind:       kind,
		URL:        pageURL,
		Detail:     detail,
		Screenshot: shot,
		Cooldown:   cooled,
	}
}

func (d *riskDetector) screenshotFunc() func(*rod.Page, string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.screenshot
}

// recordAndMaybeCooldown adds n signals and enters a cooldown once the
// threshold is reached inside the window. It returns the cooldown duration that
// was requested, or zero when no cooldown was entered.
//
// A single check that trips two independent detectors (say the URL and the
// slider container) already counts as two signals: the point of the rule is
// corroboration, not elapsed time.
func (d *riskDetector) recordAndMaybeCooldown(n int) time.Duration {
	d.mu.Lock()
	now := d.now()
	cutoff := now.Add(-d.window)
	kept := d.seen[:0]
	for _, t := range d.seen {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	d.seen = kept
	for i := 0; i < n; i++ {
		d.seen = append(d.seen, now)
	}
	count := len(d.seen)
	trip := count >= d.threshold
	cooldown := d.cooldown
	if trip {
		// Start the count again, so a cooldown is entered once per burst
		// rather than on every call for the next 15 minutes.
		d.seen = nil
	}
	d.mu.Unlock()

	if !trip {
		logrus.Warnf("risk control: %d/%d signals within %s — reporting without a cooldown", count, d.threshold, d.window)
		return 0
	}
	if cooldown == nil {
		logrus.Warnf("risk control: %d signals seen but no cooldown hook is installed", count)
		return 0
	}

	// Zero means "the gate's configured cooldown", i.e. XHS_RISK_COOLDOWN or
	// the 30 minute default. The gate owns that knob; we do not duplicate it.
	cooldown(0)
	logrus.Warnf("risk control: %d signals within %s — pacing gate placed in cooldown", count, d.window)
	return riskCooldownDurationHint()
}

// riskCooldownDurationHint is only used to fill in the error's Cooldown field
// for the operator. The authoritative deadline lives in the gate.
func riskCooldownDurationHint() time.Duration {
	if raw := os.Getenv("XHS_RISK_COOLDOWN"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Minute
}

// detail is the operator-facing sentence attached to the error.
func (d *riskDetector) detail(cooled time.Duration, signals []riskSignal) string {
	var b strings.Builder
	marks := make([]string, 0, len(signals))
	for _, s := range signals {
		marks = append(marks, s.String())
	}
	b.WriteString("matched " + strings.Join(marks, ", "))

	d.mu.Lock()
	headful := d.headful
	d.mu.Unlock()

	if headful != nil && headful() {
		// Solving the challenge is out of scope on purpose: a failed slider is
		// worse for the account than an unattempted one.
		b.WriteString("; the browser window is open — solve the verification there, then retry")
	} else {
		b.WriteString("; running headless, backing off instead of attempting the challenge")
	}

	if cooled > 0 {
		b.WriteString(fmt.Sprintf("; pacing gate in cooldown for %s", cooled))
	} else {
		b.WriteString("; first signal only, no cooldown entered yet")
	}

	return b.String()
}

// cookieSessionExpected reports whether a cookie session was saved, i.e.
// whether a login modal would be a surprise.
func cookieSessionExpected() bool {
	data, err := cookies.NewLoadCookie(cookies.GetCookiesFilePath()).LoadCookies()
	return err == nil && len(data) > 0
}

// saveRiskScreenshot writes a PNG of the challenge next to the downloaded
// images, so the operator can see what the site actually showed. This is the
// evidence that turns the guessed markers above into confirmed ones.
//
// Best effort: a failure here must not replace the risk error with a
// screenshot error.
func saveRiskScreenshot(page *rod.Page, kind string) string {
	if page == nil {
		return ""
	}

	dir := configs.GetImagesPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logrus.Warnf("risk control: cannot create screenshot directory %s: %v", dir, err)
		return ""
	}

	name := fmt.Sprintf("risk_%s_%s.png", kind, time.Now().Format("20060102_150405"))
	path := filepath.Join(dir, name)

	data, err := page.Timeout(riskCheckTimeout).Screenshot(false, nil)
	if err != nil {
		logrus.Warnf("risk control: screenshot failed: %v", err)
		return ""
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		logrus.Warnf("risk control: cannot write screenshot %s: %v", path, err)
		return ""
	}

	logrus.Warnf("risk control: screenshot saved to %s", path)
	return path
}
