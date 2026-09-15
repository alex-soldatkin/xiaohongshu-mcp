package xiaohongshu

import (
	"context"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// Click-through (issue #10, second half).
//
// navigateFrom makes a direct navigation *claim* to have come from a link. This
// file is about the cases where the claim can be dropped in favour of the
// thing itself: the page is already sitting on a surface that really does
// contain the link, so we click it and let the site write the referrer, the
// Referer header and — where the URL carries them — the query parameters.
//
// Three rules hold the whole thing together.
//
// First, falling back is automatic and silent. Not on a suitable surface, link
// missing, link points somewhere else, click did not land, page did not
// settle — every one of those returns "not done" and the caller navigates
// directly, exactly as before. A click-through that fails is never allowed to
// become a failed tool call, because the click is a nicety and the navigation
// is the job.
//
// Second, click-through only ever *replaces* a navigation, never adds one. We
// do not open explore in order to click through to somewhere else: that costs a
// page load to save nothing, and a direct navigation with a referrer is the
// better trade. The surface has to already be open.
//
// Third, nothing about the document is read until the arrival is confirmed.
// After a click that starts a navigation, the page still holds the *previous*
// document — the same trap #11 recorded for navigateFrom's pre-load risk check.
// So arrival is detected through Target.getTargetInfo (the URL, which commits
// with the navigation and needs no JS context), and only afterwards do we wait
// for load and run checkRiskControl.

// Two deliberate non-cases, recorded here so the next reader does not have to
// rediscover the reasoning.
//
// Note detail is not clicked through from explore or from search results, even
// though that is where the highest-value referrer would come from. Clicking a
// note card on the PC site opens the note as a pushState route into an overlay
// over the listing, not as a new document: there is no document request, so
// there is no Referer and no document.referrer to gain, and the DOM that
// results is the overlay's, not the standalone note page the detail and comment
// parsers are written against. The realism gain is zero and the risk is a
// second, unverified shape of every note page. What would settle it is one
// session on the live site checking whether the card click commits a
// navigation; until then, makeFeedDetailURL plus a referrer is both honest and
// correct.
//
// Ordering is identical on both routes: settle, risk check, then the
// post-navigation pause, with whatever extra in-page wait a call site does
// afterwards coming after the pause. That was already true of navigateFrom, and
// keeping it true here is what stops the two routes from being distinguishable
// by their timing shape.

// clickArrivalTimeout bounds the wait for a clicked link to actually take us
// somewhere. It is generous because a real navigation on a slow connection is
// ordinary, and cheap to get wrong in the safe direction: on timeout we simply
// navigate directly instead.
const clickArrivalTimeout = 15 * time.Second

// clickTarget is a destination that may be reachable by clicking a link on the
// page that happens to be open, together with everything needed to navigate
// there directly when it is not.
type clickTarget struct {
	// name identifies the target in logs only.
	name string
	// dest and referrer are the direct-navigation fallback.
	dest     string
	referrer string
	// wait is the settle level, used identically on both paths.
	wait navWait

	// surface reports whether the current URL is a page that plausibly
	// contains the link. It is a cheap pre-filter; the authority on whether
	// the link exists is accept, below.
	surface func(current string) bool
	// selectors are tried in order, most specific first. Every anchor they
	// match is offered to accept.
	selectors []string
	// accept reports whether an anchor's href leads where we want to go. This
	// is what stops a guessed selector from clicking the wrong thing: the link
	// has to name the destination itself.
	accept func(href string) bool
	// arrived reports whether a URL is the destination.
	arrived func(current string) bool
}

// onMainSite reports whether a URL is a page of the main site, which is where
// the sidebar lives. Every main-site page renders it, so any of them will do —
// and being already on one is the precondition for clicking rather than
// navigating.
func onMainSite(current string) bool {
	return ActiveSite().OnMainSite(current)
}

// notificationTarget is the notification centre, reached in a real session by
// clicking the bell in the sidebar.
func notificationTarget() clickTarget {
	return clickTarget{
		name:     "通知",
		dest:     urlNotification,
		referrer: urlExplore,
		wait:     navWaitLoad,
		surface:  onMainSite,
		selectors: []string{
			`div.main-container li.side-bar-component a`,
			`div.main-container a`,
		},
		accept:  func(href string) bool { return strings.Contains(href, "/notification") },
		arrived: func(current string) bool { return strings.Contains(current, "/notification") },
	}
}

// creatorPublishTarget is the publish page on the creator site, reached from
// the "发布" entry in the sidebar.
//
// The wait level is navWaitNone to match what the publish flows ask of
// navigateFrom: the publish page is heavy, and both call sites wait for load
// themselves afterwards and treat a slow load as a warning rather than a
// failure.
func creatorPublishTarget() clickTarget {
	return clickTarget{
		name:     "发布",
		dest:     urlOfPublic,
		referrer: urlExplore,
		wait:     navWaitNone,
		surface:  onMainSite,
		selectors: []string{
			`div.main-container li.side-bar-component a`,
			`div.main-container a`,
		},
		accept:  func(href string) bool { return ActiveSite().OnCreatorSite(href) },
		arrived: func(current string) bool { return ActiveSite().OnCreatorSite(current) },
	}
}

// gotoNotification opens the notification centre: by clicking the sidebar bell
// when the page is already on the main site, by navigating otherwise.
func gotoNotification(ctx context.Context, page *rod.Page) error {
	return openTarget(ctx, page, notificationTarget())
}

// gotoCreatorPublish opens the creator publish page, preferring the sidebar
// entry when the main site is already open.
func gotoCreatorPublish(ctx context.Context, page *rod.Page) error {
	return openTarget(ctx, page, creatorPublishTarget())
}

// openTarget reaches t by whichever route is available, and is the only thing
// call sites should use. Both routes end in the same state: destination loaded
// to the requested settle level, risk control checked where the document is
// current enough to judge, and the post-navigation pause spent.
func openTarget(ctx context.Context, page *rod.Page, t clickTarget) error {
	done, err := clickThrough(ctx, page, t)
	if err != nil {
		// Only a risk-control hit comes back here. It is the one thing a
		// click-through must not swallow: a captcha raised by a click is just
		// as much a captcha as one raised by a navigation.
		return err
	}
	if done {
		return nil
	}
	return navigateFrom(ctx, page, t.dest, t.referrer, t.wait)
}

// clickThrough tries to reach t by clicking a link on the page that is already
// open.
//
// It returns (true, nil) when the destination was reached, (false, nil) when
// the caller should navigate directly, and (false-or-true, err) only for an
// error the caller must not paper over — today that means risk control.
func clickThrough(ctx context.Context, page *rod.Page, t clickTarget) (bool, error) {
	current := currentURL(page)
	if t.surface == nil || !t.surface(current) {
		return false, nil
	}
	if t.arrived(current) {
		// Already there. Re-opening it is the caller's business — the list
		// actions reload to get fresh state — and clicking a link to the page
		// you are on is not what a person does.
		return false, nil
	}

	link := findClickableLink(page, t)
	if link == nil {
		logrus.Debugf("click-through: 当前页面没有通往「%s」的链接，改为直接导航", t.name)
		return false, nil
	}

	humanize.Delay(ctx, humanize.BeforeClick)
	if err := humanize.Click(link); err != nil {
		logrus.Debugf("click-through: 点击「%s」失败，改为直接导航: %v", t.name, err)
		return false, nil
	}

	// Nothing below may read the document until this returns true: until the
	// navigation commits, the page still holds the one we clicked from.
	if !awaitArrival(ctx, page, t.arrived) {
		logrus.Debugf("click-through: 点击「%s」后没有到达目标页，改为直接导航", t.name)
		return false, nil
	}

	if t.wait >= navWaitLoad {
		if err := page.WaitLoad(); err != nil {
			logrus.Debugf("click-through: 「%s」加载未完成，改为直接导航: %v", t.name, err)
			return false, nil
		}
	}

	switch t.wait {
	case navWaitDOMStable:
		if err := page.WaitDOMStable(time.Second, 0); err != nil {
			logrus.Debugf("click-through: 「%s」DOM 未稳定，改为直接导航: %v", t.name, err)
			return false, nil
		}
	case navWaitStable:
		if err := page.WaitStable(time.Second); err != nil {
			logrus.Debugf("click-through: 「%s」未稳定，改为直接导航: %v", t.name, err)
			return false, nil
		}
	}

	// Same rule as navigateFrom, for the same reason: below navWaitLoad nothing
	// has loaded yet, so the detectors would be judging the previous page. Those
	// callers (the publish flows) check again before they submit.
	if t.wait >= navWaitLoad {
		if err := checkRiskControl(page); err != nil {
			return true, err
		}
	}

	logrus.Debugf("click-through: 通过点击进入「%s」", t.name)
	humanize.Delay(ctx, humanize.AfterNavigate)
	return true, nil
}

// findClickableLink returns the first anchor on the page that leads to t and
// can be clicked without losing the page we are holding, or nil.
func findClickableLink(page *rod.Page, t clickTarget) *rod.Element {
	for _, selector := range t.selectors {
		elems, err := page.Elements(selector)
		if err != nil {
			continue
		}
		for _, elem := range elems {
			if !linkLeadsTo(elem, t.accept) {
				continue
			}
			if opensNewTab(elem) {
				// The navigation would happen in a tab we do not hold, leaving
				// this page where it is and the caller operating on the wrong
				// document. Navigating directly is both correct and cheaper
				// than adopting a popup.
				logrus.Debugf("click-through: 「%s」链接会打开新标签页，改为直接导航", t.name)
				continue
			}
			if visible, err := elem.Visible(); err != nil || !visible {
				continue
			}
			return elem
		}
	}
	return nil
}

// linkLeadsTo reports whether the element is an anchor whose href accept()
// approves. An element with no href is never clicked: without one there is
// nothing to check the destination against, and clicking on the strength of a
// guessed selector alone is how you end up somewhere else entirely.
func linkLeadsTo(elem *rod.Element, accept func(string) bool) bool {
	href, err := elem.Attribute("href")
	if err != nil || href == nil {
		return false
	}
	return accept(*href)
}

// opensNewTab reports whether following the link would open a new tab.
func opensNewTab(elem *rod.Element) bool {
	target, err := elem.Attribute("target")
	if err != nil || target == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(*target), "_blank")
}

// awaitArrival polls the target's URL until it is the destination.
//
// The URL is read through Target.getTargetInfo, which does not touch the
// document and so is safe to call while the navigation is still in flight —
// unlike anything evaluated in the page, which would answer for the document we
// just left.
func awaitArrival(ctx context.Context, page *rod.Page, arrived func(string) bool) bool {
	deadline := time.Now().Add(clickArrivalTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if info, err := page.Info(); err == nil && arrived(info.URL) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
