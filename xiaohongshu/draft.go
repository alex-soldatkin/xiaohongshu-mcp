package xiaohongshu

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// Draft support (issue #19).
//
// The creator publish footer carries two terminal actions. Everything before
// them — images, title, body, tags, visibility, schedule — is the same form
// walk, so a draft is not a second flow: it is the same walk with a different
// last click. That is why the mode is a field on the content struct rather
// than a fork of submitPublish.
//
// Where that last button lives is the whole difficulty. On the live rednote
// creator the footer is a custom element, xhs-publish-btn, whose contents sit
// in a CLOSED shadow root. Nothing reachable from page script can see inside
// it: querySelector, XPath and a full text-node walk over document.body all
// come back empty, which is why the first live attempt reported "no draft
// button" on a page that was plainly rendering one. CDP is not subject to the
// closed-mode restriction, so DOM.describeNode with pierce=true does see in,
// and that is how the button is found here.
//
// Measured on rednote 2026-09-15 (issue #18). The host carries:
//
//	is-publish="true"        is-save-draft="true"
//	submit-text="发布"        save-text="暂存离开"
//	submit-disabled="false"  save-disabled="false"
//
// and its shadow root holds div.publish-page-publish-btn wrapping
// button.ce-btn.white (the draft action) and button.ce-btn.bg-red (publish).
// Note that .publish-page-publish-btn is exactly the class findPublishButton's
// legacy light-DOM fallback looks for; on this build it has moved inside the
// closed root, so that fallback can never match here.
//
// The label is read from the save-text attribute rather than hardcoded, so the
// deployment names its own button: 暂存离开 on rednote, plausibly 存草稿
// elsewhere. draftButtonLabels is only the fallback for a build that renders
// the footer in the light DOM.

// draftButtonLabels are the labels accepted when the widget does not name its
// own. 暂存离开 is what the live rednote footer renders; the rest are wordings
// seen on creator builds that put the footer in the light DOM.
var draftButtonLabels = []string{"暂存离开", "存草稿", "保存草稿", "存为草稿"}

// draftButtonXPath matches an element whose own text node is exactly one of the
// labels. Matching the text node rather than the subtree picks the innermost
// element by construction, so we click the label a person would click instead
// of some ancestor that merely contains it.
func draftButtonXPath() string {
	parts := make([]string, 0, len(draftButtonLabels))
	for _, label := range draftButtonLabels {
		parts = append(parts, "normalize-space(text())='"+label+"'")
	}
	return "//*[" + strings.Join(parts, " or ") + "]"
}

// findDraftButton returns the draft control, a reason it is present but not
// clickable, or neither when it is not on the page yet.
func findDraftButton(page *rod.Page) (*rod.Element, string, error) {
	widgets, err := page.Elements("xhs-publish-btn")
	if err != nil {
		return nil, "", errors.Wrap(err, "查找发布按钮组件失败")
	}

	for _, widget := range widgets {
		if !isElementVisible(widget) {
			continue
		}

		// is-save-draft is the widget's own statement about whether this note
		// can be drafted at all — a scheduled or already-published note cannot.
		if v, err := widget.Attribute("is-save-draft"); err == nil && v != nil && *v == "false" {
			continue
		}
		if v, err := widget.Attribute("save-disabled"); err == nil && v != nil && *v == "true" {
			return nil, "存草稿按钮不可点击(save-disabled=true)", nil
		}

		label := ""
		if v, err := widget.Attribute("save-text"); err == nil && v != nil {
			label = strings.TrimSpace(*v)
		}

		btn, err := findShadowDraftButton(page, widget, label)
		if err != nil {
			return nil, "", err
		}
		if btn != nil {
			return btn, "", nil
		}
	}

	// Light-DOM footer: older creator builds render the buttons as ordinary
	// elements, and then the label is all there is to go on.
	elems, err := page.ElementsX(draftButtonXPath())
	if err != nil {
		return nil, "", errors.Wrap(err, "查找存草稿按钮失败")
	}
	for _, elem := range elems {
		if isElementVisible(elem) {
			return elem, "", nil
		}
	}
	return nil, "", nil
}

// findShadowDraftButton walks the widget's closed shadow root for the draft
// button. label is the widget's own save-text; when it is empty any of the
// known labels is accepted.
func findShadowDraftButton(page *rod.Page, widget *rod.Element, label string) (*rod.Element, error) {
	node, err := widget.Describe(-1, true)
	if err != nil {
		return nil, errors.Wrap(err, "读取发布按钮组件的 shadow DOM 失败")
	}

	match := func(text string) bool {
		if label != "" {
			return text == label
		}
		for _, l := range draftButtonLabels {
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
		return nil, errors.Wrap(err, "解析存草稿按钮节点失败")
	}
	return elem, nil
}

// findButtonNode searches a described node tree — children and shadow roots
// alike — for a BUTTON whose own text matches.
func findButtonNode(node *proto.DOMNode, match func(text string) bool) *proto.DOMNode {
	if node == nil {
		return nil
	}

	if strings.EqualFold(node.NodeName, "button") && match(nodeOwnText(node)) {
		return node
	}

	for _, child := range node.Children {
		if got := findButtonNode(child, match); got != nil {
			return got
		}
	}
	for _, root := range node.ShadowRoots {
		if got := findButtonNode(root, match); got != nil {
			return got
		}
	}
	return nil
}

// nodeOwnText joins a node's direct text children, which is where a button's
// label lives.
func nodeOwnText(node *proto.DOMNode) string {
	var b strings.Builder
	for _, child := range node.Children {
		if child.NodeType == 3 { // Node.TEXT_NODE
			b.WriteString(child.NodeValue)
		}
	}
	return strings.TrimSpace(b.String())
}

// waitForDraftButton polls for the draft control. The footer renders only once
// the upload has produced a form, so a short absence is normal.
func waitForDraftButton(page *rod.Page, maxWait time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(maxWait)
	var lastDisabledReason string

	for {
		btn, disabledReason, err := findDraftButton(page)
		if err == nil && btn != nil {
			return btn, nil
		}
		if disabledReason != "" {
			lastDisabledReason = disabledReason
		}
		if time.Now().After(deadline) {
			if lastDisabledReason != "" {
				return nil, errors.Errorf("等待存草稿按钮可点击超时: %s", lastDisabledReason)
			}
			// Name what the footer does advertise. A button that moved or was
			// renamed is a selector fact worth reporting, and this is the only
			// place that can report it.
			return nil, errors.Errorf("未找到存草稿按钮；发布按钮组件状态: %s", describePublishWidgets(page))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// describePublishWidgets reports the footer widget's attributes so a missing
// draft button produces a diagnosis rather than a bare timeout.
func describePublishWidgets(page *rod.Page) string {
	widgets, err := page.Elements("xhs-publish-btn")
	if err != nil {
		return "(读取发布按钮组件失败: " + err.Error() + ")"
	}
	if len(widgets) == 0 {
		return "(页面上没有 xhs-publish-btn 组件)"
	}

	var seen []string
	for _, widget := range widgets {
		var attrs []string
		for _, name := range []string{"is-publish", "is-save-draft", "submit-text", "save-text", "submit-disabled", "save-disabled"} {
			v, err := widget.Attribute(name)
			if err != nil || v == nil {
				continue
			}
			attrs = append(attrs, name+"="+*v)
		}
		seen = append(seen, "["+strings.Join(attrs, " ")+"]")
	}
	return strings.Join(seen, " ")
}

// draftSavedToasts are the toast texts the creator shows on a successful save.
// Measured on rednote: a d-new-toast / d-toast-notice reading 保存成功, visible
// for roughly five seconds.
var draftSavedToasts = []string{"保存成功", "草稿保存成功", "已保存"}

// saveDraft is the draft terminal step: the risk check that guards every
// submit, the click, and a confirmation that the draft actually landed.
//
// before/knownBefore are the 草稿箱 count read on the upload page before the
// form existed — the counter is not on the form page, so it has to be carried
// in from the start of the action.
func saveDraft(page *rod.Page, before int, knownBefore bool) error {
	// Same guard as clickPublishButton: a draft saved into a challenged
	// session is a draft that quietly does not exist (issue #11).
	if err := checkRiskControl(page); err != nil {
		return err
	}

	btn, err := waitForDraftButton(page, 15*time.Second)
	if err != nil {
		return err
	}
	if err := humanize.Click(btn); err != nil {
		return errors.Wrap(err, "点击存草稿按钮失败")
	}
	slog.Info("已点击存草稿按钮", "draft_count_before", before, "count_readable", knownBefore)

	return waitDraftSaved(page, before, knownBefore, 30*time.Second)
}

// draftSavedToastShown reports whether the success toast is on screen.
func draftSavedToastShown(page *rod.Page) bool {
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
		for _, want := range draftSavedToasts {
			if strings.Contains(text, want) {
				return true
			}
		}
	}
	return false
}

// waitDraftSaved confirms the draft landed rather than trusting the click.
//
// Three independent signals, because what the creator does on a save differs by
// deployment and the first live run got this wrong. On rednote the button says
// 暂存离开 — save and leave — but it does NOT navigate: the URL stays on
// /publish/publish, the form is torn down back to the upload page, and a
// 保存成功 toast appears for about five seconds. So a URL check alone reports
// failure on a save that plainly worked. The counter is the signal an operator
// can check by hand, and it becomes readable again exactly when the form goes
// away, so it doubles as proof the form was dismantled rather than merely
// clicked at.
func waitDraftSaved(page *rod.Page, before int, knownBefore bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	toastSeen := false

	for {
		if !toastSeen && draftSavedToastShown(page) {
			slog.Info("存草稿成功：页面提示保存成功")
			toastSeen = true
		}

		if after, ok := readDraftCount(page); ok {
			switch {
			case knownBefore && after > before:
				slog.Info("存草稿成功：草稿箱计数已增加", "before", before, "after", after)
				return nil
			case toastSeen:
				// Counter readable again plus a success toast: the form was
				// dismantled and the site said it saved. Enough on a build
				// whose counter lags, or when the count was never readable.
				slog.Info("存草稿成功：已回到上传页且提示保存成功", "draft_count", after)
				return nil
			}
		}

		if info, err := page.Info(); err == nil && !strings.Contains(info.URL, "/publish/publish") {
			slog.Info("存草稿成功：已跳转离开发布页", "url", info.URL)
			return nil
		}

		if time.Now().After(deadline) {
			after, ok := readDraftCount(page)
			return errors.Errorf("存草稿未确认成功：未见保存成功提示，草稿箱计数也未增加"+
				"(before=%d known=%v after=%d readable=%v)", before, knownBefore, after, ok)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// readDraftCount reads the 草稿箱 counter the creator page renders in the
// upload header, e.g. "草稿箱(1)" in span.draft-title. The second return value
// distinguishes "zero drafts" from "counter not on this page", which matters:
// the counter is only in the DOM before the upload replaces the header with
// the form, and treating its absence as 0 would make any later read look like
// a success.
func readDraftCount(page *rod.Page) (int, bool) {
	// span.draft-title is the header counter measured live. Prefer it: after a
	// save the page also renders a drafts panel whose own 草稿箱 heading carries
	// no number, and the generic search would have to skip past it.
	if elems, err := page.Elements("span.draft-title"); err == nil {
		for _, elem := range elems {
			text, err := elem.Text()
			if err != nil {
				continue
			}
			if n, ok := parseDraftCount(text); ok {
				return n, true
			}
		}
	}

	elems, err := page.ElementsX("//*[contains(text(),'草稿箱')]")
	if err != nil {
		return 0, false
	}

	for _, elem := range elems {
		text, err := elem.Text()
		if err != nil {
			continue
		}
		if n, ok := parseDraftCount(text); ok {
			return n, true
		}
	}
	return 0, false
}

// parseDraftCount pulls the number out of a 草稿箱 label. The counter is
// rendered in brackets after the word, and the site uses both ASCII and
// fullwidth brackets depending on the build.
func parseDraftCount(text string) (int, bool) {
	idx := strings.Index(text, "草稿箱")
	if idx < 0 {
		return 0, false
	}
	rest := text[idx+len("草稿箱"):]

	digits := strings.Builder{}
	started := false
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
			started = true
			continue
		}
		if started {
			break
		}
		// Only brackets and spaces may sit between the word and its count; any
		// other character means this label carries no number.
		if r != '(' && r != ')' && r != '（' && r != '）' &&
			r != ' ' && r != '\u00a0' && r != '\u3000' {
			return 0, false
		}
	}

	if !started {
		// "草稿箱" with no number at all is the empty-drafts rendering on some
		// builds. That is a readable zero, not an unreadable counter.
		return 0, true
	}

	n, err := strconv.Atoi(digits.String())
	if err != nil {
		return 0, false
	}
	return n, true
}
