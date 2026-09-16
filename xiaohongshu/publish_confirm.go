package xiaohongshu

import (
	"log/slog"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
)

// Publish confirmation and the publish button itself (issue #8).
//
// Two things were wrong here and both could only hurt a real publish.
//
// 1. Success was inferred from navigation alone: the old waitPublishSuccess
//    polled the URL and called anything still on /publish/publish a failure.
//    The draft path proved that assumption wrong on rednote — 暂存离开 saves,
//    shows a 保存成功 toast, tears the form down to the upload page, and never
//    changes the URL (issue #19). Nobody had checked whether 发布 behaves the
//    same way, and if it does then every successful publish was reported as a
//    failure. The obvious response to a reported failure is a retry, which
//    publishes the same note twice. So this now takes the draft path's three
//    independent signals instead of trusting one.
//
// 2. The button was clicked by arithmetic. findPublishButton's light-DOM
//    selector .publish-page-publish-btn button.bg-red cannot match on rednote,
//    because that markup lives inside xhs-publish-btn's CLOSED shadow root
//    (issue #16). What actually clicked 发布 was clickPublishWidget, aiming at
//    65% of the host's width — inside the button with about a 4% margin, and
//    only for as long as the widget's layout stays exactly as measured. The
//    button is now found the way the draft button is: DOM.describeNode with
//    pierce=true walks into the closed root and ElementFromNode hands back an
//    ordinary element. The coordinate click survives only as a last resort.
//
// Labels come from the widget's own attributes — submit-text on rednote is
// 发布 and save-text is 暂存离开, not 存草稿 — so nothing here assumes a
// particular Chinese phrase when the page is willing to name its own buttons.

// publishSubmitLabels are the labels accepted when the widget does not carry a
// submit-text attribute. Only a fallback: rednote names its own button.
var publishSubmitLabels = []string{"发布", "立即发布", "定时发布"}

// publishSuccessToasts are the toast texts a creator build shows on a
// successful publish. Superset of what any single deployment renders; the
// toast is one of three signals, so an unmatched wording costs nothing.
var publishSuccessToasts = []string{"发布成功", "发布完成", "已发布", "定时发布成功", "发布中"}

// findShadowPublishButton walks the widget's closed shadow root for the submit
// button. label is the widget's own submit-text; when it is empty any of the
// known labels is accepted.
func findShadowPublishButton(page *rod.Page, widget *rod.Element, label string) (*rod.Element, error) {
	node, err := widget.Describe(-1, true)
	if err != nil {
		return nil, errors.Wrap(err, "读取发布按钮组件的 shadow DOM 失败")
	}

	match := func(text string) bool {
		if label != "" {
			return text == label
		}
		for _, l := range publishSubmitLabels {
			if text == l {
				return true
			}
		}
		return false
	}

	found := findButtonNode(node, match)
	if found == nil {
		return nil, nil
	}

	elem, err := page.ElementFromNode(found)
	if err != nil {
		return nil, errors.Wrap(err, "解析发布按钮节点失败")
	}
	return elem, nil
}

// publishFormPresent reports whether the publish form is still on screen.
//
// The form is what a failed publish leaves behind, so its disappearance is the
// state change that distinguishes "the site accepted this" from "the site is
// still holding the note". Any one of the footer widget, the title input and
// the body editor being visible counts as still present.
func publishFormPresent(page *rod.Page) bool {
	for _, selector := range []string{"xhs-publish-btn", "div.d-input input", "div.ql-editor", "div.tiptap"} {
		elems, err := page.Elements(selector)
		if err != nil {
			continue
		}
		for _, elem := range elems {
			if isElementVisible(elem) {
				return true
			}
		}
	}
	return false
}

// publishSuccessToastShown reports whether a success toast is on screen. Same
// toast containers the draft path measured live.
func publishSuccessToastShown(page *rod.Page) bool {
	elems, err := page.Elements(".d-toast-notice, .d-new-toast, .d-toast-wrapper")
	if err != nil {
		return false
	}
	for _, elem := range elems {
		if !isElementVisible(elem) {
			continue
		}
		text, err := elem.Text()
		if err != nil {
			continue
		}
		text = strings.TrimSpace(text)
		for _, want := range publishSuccessToasts {
			if strings.Contains(text, want) {
				return true
			}
		}
	}
	return false
}

// waitPublishSuccess confirms the note actually went out, on three independent
// signals rather than on navigation alone:
//
//   - the URL leaves /publish/publish (deployments that navigate away);
//   - a 发布成功-class toast appears;
//   - the publish form is torn down and the upload page comes back, which is
//     what the draft path does on rednote and what a failed publish never does
//     — a rejected note keeps its form, with the error on it.
//
// Form teardown is accepted on its own because it cannot happen while the site
// is still refusing the note. The toast is accepted on its own because it is
// the site saying so in as many words. Both are logged by name, so a live run
// records which signal a deployment actually produces.
func waitPublishSuccess(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// The form is normally still up at the moment of the click; if it never
	// was, teardown proves nothing and only the other two signals count.
	formSeen := publishFormPresent(page)

	for {
		if info, err := page.Info(); err == nil && !strings.Contains(info.URL, "/publish/publish") {
			slog.Info("发布成功：已跳转离开发布页", "url", info.URL)
			return nil
		}

		if publishSuccessToastShown(page) {
			slog.Info("发布成功：页面提示发布成功")
			return nil
		}

		if formSeen && !publishFormPresent(page) {
			count, readable := readDraftCount(page)
			slog.Info("发布成功：发布表单已收起，回到上传页",
				"url_changed", false, "draft_count", count, "count_readable", readable)
			return nil
		}

		if time.Now().After(deadline) {
			url := ""
			if info, err := page.Info(); err == nil {
				url = info.URL
			}
			return errors.Errorf("发布未确认成功：未跳转、未见发布成功提示、发布表单仍在页面上"+
				"(url=%s form_present=%v form_seen_at_click=%v)", url, publishFormPresent(page), formSeen)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
