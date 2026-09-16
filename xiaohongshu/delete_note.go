package xiaohongshu

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// Deleting a published note (issue #20).
//
// This is the only action in the project that destroys content, so it is built
// to be hard to point at the wrong thing. The note id is required and is the
// only way to name a note: there is no "the most recent one" and no "the last
// one I published", because the caller this tool is built for is an agent, and
// an agent working from a confused instruction must not be able to resolve a
// vague phrase into somebody's real note. The id has to come from a listing the
// caller already read.
//
// Measured on the live rednote creator, 2026-09-16:
//
//   - the list is at creator.<domain>/new/note-manager, tabs 全部 / 已发布 /
//     审核中 / 未通过, cards rendered as div.note-card;
//   - the only place a card states its note id is its data-impression
//     attribute, a JSON analytics payload carrying noteTarget.value.noteId.
//     There is no href, no data-note-id and no id on the card itself;
//   - the delete control is span.note-card__action-btn--del in the card header,
//     icon-only with no text at all, so it cannot be found by label.
//
// Because the card carries no text form of its id, matching is done on the
// parsed attribute rather than on anything visible. That is deliberate: the
// visible fields are the title and the publish time, and both can repeat.

// notesManagerPath is the 笔记管理 list. Same path on both deployments;
// confirmed live on rednote.
const notesManagerPath = "/new/note-manager"

// deleteConfirmLabels are the labels accepted on the confirmation dialog's
// affirmative button. Ordered most specific first so that a dialog offering
// both 删除 and 确定 is answered with the one that names the action.
var deleteConfirmLabels = []string{"确认删除", "删除", "确定", "确认"}

// deleteSuccessToasts are the toast texts seen on a successful delete.
var deleteSuccessToasts = []string{"删除成功", "已删除"}

// NotesManagerURL is the creator centre's note list.
func (s Site) NotesManagerURL() string {
	return "https://" + s.CreatorHost() + notesManagerPath
}

// DeleteNoteAction deletes one published note from 笔记管理.
type DeleteNoteAction struct {
	page *rod.Page
}

// NewDeleteNoteAction opens the note list.
func NewDeleteNoteAction(ctx context.Context, page *rod.Page) (*DeleteNoteAction, error) {
	pp := page.Timeout(120 * time.Second)

	if err := navigateFrom(ctx, pp, ActiveSite().NotesManagerURL(), ActiveSite().CreatorPublish(), navWaitLoad); err != nil {
		return nil, errors.Wrap(err, "导航到笔记管理页失败")
	}
	if err := pp.WaitDOMStable(time.Second, 0.1); err != nil {
		slog.Warn("笔记管理页 DOM 未稳定，继续尝试", "error", err)
	}
	time.Sleep(2 * time.Second)

	return &DeleteNoteAction{page: pp}, nil
}

// Delete removes the note with the given id. It fails rather than guesses: an
// id that is not on the list is an error, never "delete something else".
func (a *DeleteNoteAction) Delete(ctx context.Context, noteID string) error {
	noteID = strings.TrimSpace(noteID)
	if noteID == "" {
		return errors.New("必须指定要删除的笔记 ID")
	}

	page := a.page.Context(ctx)

	// A challenge here means the delete may silently not happen, same reason
	// the publish path checks before its terminal click (issue #11).
	if err := checkRiskControl(page); err != nil {
		return err
	}

	card, err := findNoteCard(page, noteID)
	if err != nil {
		return err
	}

	btn, err := card.Element("span.note-card__action-btn--del")
	if err != nil {
		return errors.Wrapf(err, "笔记 %s 没有删除按钮（可能是审核中或不可删除的笔记）", noteID)
	}
	if err := humanize.Click(btn); err != nil {
		return errors.Wrap(err, "点击删除按钮失败")
	}
	slog.Info("已点击笔记删除按钮", "note_id", noteID)

	if err := confirmDelete(ctx, page); err != nil {
		return err
	}

	// 点完确认只是"点过了"。列表把卡片撤下来是乐观更新，跟服务端删没删没有
	// 关系 —— 实测过一次：卡片当场消失、接口回了成功，一小时后笔记还在
	// 笔记管理里。删除这种动作，报错的方向必须是宁可多报失败。
	waitNoteRemovedOptimistically(page, noteID, 15*time.Second)

	return a.verifyDeleted(ctx, noteID)
}

// verifyDeleted 重新加载笔记管理页，确认这篇笔记真的不在了。
//
// 这是整条删除链路里唯一可信的信号：前端会在请求还没回来之前就把卡片撤掉，
// 所以"卡片消失"只是必要条件。重新加载一次，列表是服务端重新给的，它说没有
// 才算没有。重试是因为删除在服务端可能有短暂延迟；重试一次删除本身是幂等的，
// 不像发布重试会多发一篇。
func (a *DeleteNoteAction) verifyDeleted(ctx context.Context, noteID string) error {
	page := a.page.Context(ctx)

	var lastIDs []string
	for attempt := 1; attempt <= 3; attempt++ {
		time.Sleep(3 * time.Second)

		if err := navigateFrom(ctx, page, ActiveSite().NotesManagerURL(), ActiveSite().NotesManagerURL(), navWaitLoad); err != nil {
			return errors.Wrap(err, "重新加载笔记管理页失败")
		}
		if err := page.WaitDOMStable(time.Second, 0.1); err != nil {
			slog.Warn("笔记管理页 DOM 未稳定", "error", err)
		}
		time.Sleep(3 * time.Second)

		if !notesListLoaded(page) {
			slog.Warn("笔记管理列表没有渲染出来，重试", "attempt", attempt)
			continue
		}

		ids, err := noteCardIDs(page)
		if err != nil {
			slog.Warn("读取笔记列表失败，重试", "attempt", attempt, "error", err)
			continue
		}
		lastIDs = ids

		if !containsID(ids, noteID) {
			slog.Info("删除已生效：重新加载后笔记不在笔记管理列表里",
				"note_id", noteID, "remaining", len(ids), "attempt", attempt)
			return nil
		}
		slog.Warn("重新加载后笔记仍在列表里，重试", "note_id", noteID, "attempt", attempt)
	}

	return errors.Errorf("删除未生效：确认弹窗已点击，但重新加载笔记管理后笔记 %s 仍在列表里"+
		"（当前列表 %d 篇：%s）。前端会在请求回来前就把卡片撤下，所以卡片消失不算数",
		noteID, len(lastIDs), strings.Join(lastIDs, ", "))
}

// notesListLoaded 判断笔记管理列表是否已经渲染。用来区分"这篇笔记没了"和
// "整页还没出来"——后者当成删除成功会是最糟的误判。
func notesListLoaded(page *rod.Page) bool {
	elems, err := page.Elements("div.tab-item")
	if err != nil {
		return false
	}
	for _, elem := range elems {
		if isElementVisible(elem) {
			return true
		}
	}
	return false
}

// noteCardIDs returns the note id of every card currently rendered, in
// document order, so that a miss can name what was actually on the page.
func noteCardIDs(page *rod.Page) ([]string, error) {
	cards, err := page.Elements("div.note-card")
	if err != nil {
		return nil, errors.Wrap(err, "读取笔记列表失败")
	}

	ids := make([]string, 0, len(cards))
	for _, card := range cards {
		id, err := noteCardID(card)
		if err != nil || id == "" {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// findNoteCard locates the card for a note id, scrolling to pull in more of a
// lazily rendered list before giving up.
func findNoteCard(page *rod.Page, noteID string) (*rod.Element, error) {
	deadline := time.Now().Add(20 * time.Second)

	for {
		cards, err := page.Elements("div.note-card")
		if err != nil {
			return nil, errors.Wrap(err, "读取笔记列表失败")
		}

		for _, card := range cards {
			id, err := noteCardID(card)
			if err != nil {
				continue
			}
			if id == noteID {
				return card, nil
			}
		}

		if time.Now().After(deadline) {
			ids, _ := noteCardIDs(page)
			return nil, errors.Errorf("笔记管理页上没有找到笔记 %s（页面上共 %d 篇：%s）",
				noteID, len(ids), strings.Join(ids, ", "))
		}

		// The list loads more as it is scrolled. Reaching the bottom is the
		// only way to be sure an absent id is really absent.
		if err := page.Mouse.Scroll(0, 800, 3); err != nil {
			slog.Warn("滚动笔记列表失败", "error", err)
		}
		time.Sleep(time.Second)
	}
}

// noteCardID pulls the note id out of a card's data-impression payload, which
// is the only place the card states it.
func noteCardID(card *rod.Element) (string, error) {
	attr, err := card.Attribute("data-impression")
	if err != nil || attr == nil {
		return "", errors.New("笔记卡片没有 data-impression 属性")
	}
	return parseNoteCardImpression(*attr)
}

// impressionPayload is the slice of the analytics payload that names the note.
type impressionPayload struct {
	NoteTarget struct {
		Value struct {
			NoteID string `json:"noteId"`
		} `json:"value"`
	} `json:"noteTarget"`
}

// parseNoteCardImpression reads the note id out of the card's data-impression
// JSON. Split out so it can be tested against the live payload shape without a
// browser.
func parseNoteCardImpression(raw string) (string, error) {
	var payload impressionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", errors.Wrap(err, "解析笔记卡片 data-impression 失败")
	}
	id := strings.TrimSpace(payload.NoteTarget.Value.NoteID)
	if id == "" {
		return "", errors.New("笔记卡片 data-impression 中没有 noteId")
	}
	return id, nil
}

// confirmDelete answers the confirmation dialog. A delete that is never
// confirmed leaves the note in place, so a missing dialog is an error rather
// than something to shrug at — but a build that deletes without asking is
// handled too, by treating the card's disappearance as the answer.
func confirmDelete(ctx context.Context, page *rod.Page) error {
	deadline := time.Now().Add(10 * time.Second)

	for {
		btn, dialogText := findDeleteConfirmButton(page)
		if btn != nil {
			// Log what the dialog said before answering it: on a build nobody
			// has watched, this line is the only record of what was agreed to.
			slog.Info("删除确认弹窗", "text", dialogText)

			// 弹窗弹出来的同时页面还在发 permission/validate?function_type=delete
			// 的前置校验，弹窗本身也有淡入动画。抢在这之前点确定，观察到过一次
			// 确认被吞掉：卡片撤下了，服务端没删。停一下再点，人看弹窗本来也要
			// 一两秒。
			humanize.Delay(ctx, humanize.Reading)
			if err := humanize.Click(btn); err != nil {
				return errors.Wrap(err, "点击删除确认按钮失败")
			}
			return nil
		}

		if time.Now().After(deadline) {
			return errors.New("未找到删除确认弹窗，删除未执行")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findDeleteConfirmButton returns the affirmative button of a visible modal,
// together with the modal's text for the log.
func findDeleteConfirmButton(page *rod.Page) (*rod.Element, string) {
	modals, err := page.Elements(".d-modal-mask, .d-modal, .d-dialog, div[role='dialog']")
	if err != nil {
		return nil, ""
	}

	for _, modal := range modals {
		if !isElementVisible(modal) {
			continue
		}
		text, _ := modal.Text()
		text = strings.TrimSpace(text)

		buttons, err := modal.Elements("button, .d-button, .ce-btn, span.btn")
		if err != nil {
			continue
		}
		for _, label := range deleteConfirmLabels {
			for _, btn := range buttons {
				if !isElementVisible(btn) {
					continue
				}
				bt, err := btn.Text()
				if err != nil {
					continue
				}
				if strings.TrimSpace(bt) == label {
					return btn, text
				}
			}
		}
	}
	return nil, ""
}

// waitNoteRemovedOptimistically waits for the page's own reaction to the
// confirm click: the success toast, or the card leaving the list. Neither
// proves anything on its own — both are client-side — so this reports nothing
// and only keeps verifyDeleted from reloading into a page that is still
// mid-animation.
func waitNoteRemovedOptimistically(page *rod.Page, noteID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if deleteToastShown(page) {
			slog.Info("页面提示删除成功（仅前端信号，仍需复核）", "note_id", noteID)
			return
		}
		if ids, err := noteCardIDs(page); err == nil && len(ids) > 0 && !containsID(ids, noteID) {
			slog.Info("笔记卡片已从列表移除（仅前端信号，仍需复核）", "note_id", noteID, "remaining", len(ids))
			return
		}
		time.Sleep(time.Second)
	}
	slog.Warn("点击确认后没等到任何前端反馈，继续复核服务端状态", "note_id", noteID)
}

// containsID reports whether a note id is in a list of them.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// deleteToastShown reports whether the delete success toast is on screen. Kept
// separate from the list check so a build that removes the card only after a
// reload still confirms.
func deleteToastShown(page *rod.Page) bool {
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
		for _, want := range deleteSuccessToasts {
			if strings.Contains(strings.TrimSpace(text), want) {
				return true
			}
		}
	}
	return false
}
