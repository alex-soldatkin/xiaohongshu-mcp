package xiaohongshu

import (
	"context"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// Cookie consent (issue #18, workstream E).
//
// rednote greets a profile that has not consented with a banner and, behind
// it, div.cookie-banner-overlay: position fixed, the exact size of the
// viewport, z-index 999998, pointer-events auto. It is a real click
// interceptor — document.elementsFromPoint returns the overlay as the topmost
// element over the feed, over the sidebar, everywhere — so a session that is
// perfectly logged in still cannot click anything, and every click-driven
// action fails with an interactability timeout rather than with anything that
// names the cause.
//
// Consent is stored in localStorage (xhs_cookie_consent), which lives in the
// browser profile and not in cookies.json. That is the trap: restoring a good
// session file into a fresh profile reproduces the blocked state exactly. Our
// own long-lived profile consented during the human login, so the banner never
// appears in normal operation here — this is insurance for the next fresh
// profile, and it is a no-op whenever the element is absent.
//
// The mainland site shows no such banner, so SiteXiaohongshu leaves the
// selector empty and nothing below ever runs there.

// consentDismissTimeout bounds the wait for the banner to leave the DOM after
// the accept button is clicked. Measured removal is immediate.
const consentDismissTimeout = 5 * time.Second

// dismissConsent accepts the cookie banner when one is up.
//
// Absent element, absent selector: no-op, and no cost beyond one querySelector.
// A banner that is found but cannot be dismissed is an error, because
// everything the caller is about to do will otherwise fail for a reason the
// caller cannot see.
func dismissConsent(ctx context.Context, page *rod.Page) error {
	selector := ActiveSite().ConsentAcceptSelector
	if selector == "" {
		return nil
	}

	has, accept, err := page.Has(selector)
	if err != nil || !has {
		return nil
	}

	humanize.Delay(ctx, humanize.BeforeClick)
	if err := humanize.Click(accept); err != nil {
		return fmt.Errorf("点击 cookie 同意按钮失败: %w", err)
	}

	// Confirm the thing actually went away rather than trusting the click:
	// a banner still on screen is the whole problem this function exists for.
	if err := waitGone(page, selector, consentDismissTimeout); err != nil {
		return fmt.Errorf("cookie 同意横幅未消失: %w", err)
	}

	logrus.WithField("selector", selector).Info("已接受 cookie 横幅")
	humanize.Delay(ctx, humanize.AfterClick)
	return nil
}

// waitGone polls until the selector matches nothing, or the deadline passes.
func waitGone(page *rod.Page, selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		has, _, err := page.Has(selector)
		if err != nil {
			return err
		}
		if !has {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s 在 %s 后仍然存在", selector, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
