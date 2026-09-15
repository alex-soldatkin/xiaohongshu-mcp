package xiaohongshu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// Fixed landing pages on the site, used both as navigation targets and as
// referrers for the pages they link to.
//
// Vars, not consts: SetSite rewrites them at startup from the active
// deployment profile (see site.go), which keeps every use of them a plain
// identifier. Read-only after that.
var (
	urlHome         = SiteXiaohongshu.Home()
	urlExplore      = SiteXiaohongshu.Explore()
	urlNotification = SiteXiaohongshu.Notification()
)

// navWait says how settled the page has to be before navigateFrom returns.
// The levels are cumulative and mirror what each call site used to do for
// itself with MustWaitLoad / MustWaitDOMStable / MustWaitStable.
type navWait int

const (
	navWaitNone      navWait = iota // return as soon as the navigation is accepted
	navWaitLoad                     // load event
	navWaitDOMStable                // + DOM stops changing
	navWaitStable                   // + DOM and network both quiet
)

// navigateFrom navigates page to url as though the user had followed a link on
// referrer, then waits and pauses the way a reader would.
//
// This is the whole point of not using page.Navigate: Page.navigate carries a
// referrer, and the browser turns it into both the Referer request header and
// document.referrer on the destination. A plain page.Navigate sends neither, so
// every deep link looks like a session that teleported to a note it could not
// have found. Passing an empty referrer means the opposite claim — that the
// user typed the address — so the transition type follows suit.
//
// Cross-origin referrers (notably to the creator centre) are trimmed to
// the bare origin by the default referrer policy. That is ordinary browser
// behaviour, not a bug here.
func navigateFrom(ctx context.Context, page *rod.Page, url, referrer string, wait navWait) error {
	transition := proto.PageTransitionTypeLink
	if referrer == "" {
		transition = proto.PageTransitionTypeTyped
	}

	// Mirror rod's Page.Navigate: drop whatever is still loading first.
	_ = page.StopLoading()

	res, err := proto.PageNavigate{
		URL:            url,
		Referrer:       referrer,
		TransitionType: transition,
	}.Call(page)
	if err != nil {
		return fmt.Errorf("导航到 %s 失败: %w", url, err)
	}
	if res.ErrorText != "" {
		return fmt.Errorf("导航到 %s 失败: %s", url, res.ErrorText)
	}

	// rod's Page.Navigate also drops its cached JS execution context id here,
	// but unsetJSCtxID is unexported. It costs nothing: Page.Evaluate already
	// retries on ErrCtxNotFound after unsetting the stale id itself, so the
	// first eval after the document swap repairs the cache.
	if wait >= navWaitLoad {
		if err := page.WaitLoad(); err != nil {
			return fmt.Errorf("等待 %s 加载失败: %w", url, err)
		}
	}

	switch wait {
	case navWaitDOMStable:
		if err := page.WaitDOMStable(time.Second, 0); err != nil {
			return fmt.Errorf("等待 %s DOM 稳定失败: %w", url, err)
		}
	case navWaitStable:
		if err := page.WaitStable(time.Second); err != nil {
			return fmt.Errorf("等待 %s 稳定失败: %w", url, err)
		}
	}

	// Every deep link lands here, which makes this the one place where a
	// redirect to a captcha or a security interstitial can be caught for the
	// whole codebase (issue #11). A challenge reported here is the difference
	// between "flagged" and "the selector we were waiting for timed out".
	//
	// navWaitNone callers are skipped on purpose: nothing has loaded yet, so the
	// document still belongs to the previous page and judging it would be
	// judging the wrong page. Those callers (the publish flows) wait for load
	// themselves and are covered by the check before their submit.
	if wait >= navWaitLoad {
		if err := checkRiskControl(page); err != nil {
			return err
		}
	}

	humanize.Delay(ctx, humanize.AfterNavigate)
	return nil
}

// currentURL reads the current address, falling back to explore. It is only
// ever used as a referrer, so failing to read it is not worth failing a call.
func currentURL(page *rod.Page) string {
	info, err := page.Info()
	if err != nil || info.URL == "" {
		return urlExplore
	}
	return info.URL
}

type NavigateAction struct {
	page *rod.Page
}

func NewNavigate(page *rod.Page) *NavigateAction {
	return &NavigateAction{page: page}
}

func (n *NavigateAction) ToExplorePage(ctx context.Context) error {
	page := n.page.Context(ctx).Timeout(60 * time.Second) // 加超时保护，避免 MustNavigate/MustWaitStable 无限挂

	// No referrer: explore is where a session starts, not somewhere it is linked to.
	if err := navigateFrom(ctx, page, urlExplore, "", navWaitLoad); err != nil {
		return err
	}
	page.MustElement(`div#app`)

	return nil
}

// profileSidebarLink is the "我" entry in the main-site sidebar. It is the only
// route to one's own profile that does not require knowing one's own user id,
// which is why this action clicks rather than navigates.
const profileSidebarLink = `div.main-container li.user.side-bar-component a.link-wrapper span.channel`

func (n *NavigateAction) ToProfilePage(ctx context.Context) error {
	page := n.page.Context(ctx).Timeout(60 * time.Second) // 加超时保护，避免 MustNavigate/MustWaitStable 无限挂

	// Only load explore when the sidebar is not already in front of us. With a
	// long-lived page (#6) it often is, and opening explore purely to click a
	// link that is on screen already spends a page load for nothing.
	if !n.sidebarIsOpen(page) {
		if err := n.ToExplorePage(ctx); err != nil {
			return err
		}
	}

	page.MustWaitStable()

	// Find and click the "我" channel link in sidebar
	profileLink := page.MustElement(profileSidebarLink)
	humanize.Delay(ctx, humanize.BeforeClick)
	if err := humanize.Click(profileLink); err != nil {
		return err
	}

	// Wait for the click to land before touching the document: until the
	// navigation commits, the page still answers for the sidebar we clicked
	// from. Failing to see the profile URL is not fatal — the load wait below
	// is the same one this action always did.
	awaitArrival(ctx, page, func(current string) bool {
		return strings.Contains(current, "/user/profile")
	})

	page.MustWaitLoad()

	// Parity with navigateFrom: a challenge raised by a click is still a
	// challenge, and must not be reported as "the selector timed out".
	return checkRiskControl(page)
}

// sidebarIsOpen reports whether the profile entry is already on screen, meaning
// a main-site page is open and rendered.
func (n *NavigateAction) sidebarIsOpen(page *rod.Page) bool {
	if !onMainSite(currentURL(page)) {
		return false
	}
	has, _, err := page.Has(profileSidebarLink)
	return err == nil && has
}
