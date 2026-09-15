//go:build integration

package humanize

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

// 三种目标元素：publish 是 tiptap contenteditable，notification_reply 是 textarea，
// 搜索框和日期框是 input。
const typeFixtureHTML = `<!doctype html>
<meta charset="utf-8">
<body>
<input id="a">
<textarea id="t"></textarea>
<div id="b" contenteditable="true"></div>
<script>
window.__ev = [];
for (const t of ['keydown','keypress','keyup','beforeinput','input',
                 'compositionstart','compositionupdate','compositionend']) {
  window.addEventListener(t, function (e) {
    window.__ev.push({
      type: e.type,
      target: (e.target && e.target.id) || '',
      keyCode: (e.keyCode === undefined || e.keyCode === null) ? -1 : e.keyCode,
      key: e.key || '',
      inputType: e.inputType || ''
    });
  }, true);
}
</script>
</body>`

type typedEvent struct {
	Type      string `json:"type"`
	Target    string `json:"target"`
	KeyCode   int    `json:"keyCode"`
	Key       string `json:"key"`
	InputType string `json:"inputType"`
}

func openTypeFixture(t *testing.T, b *rod.Browser) *rod.Page {
	t.Helper()

	page := b.MustPage().Timeout(60 * time.Second)
	// data: URL 当 fixture，不落文件。显式超时，免得导航卡死时空等。
	if err := page.Navigate("data:text/html;charset=utf-8," + url.PathEscape(typeFixtureHTML)); err != nil {
		t.Fatalf("加载 fixture 失败: %v", err)
	}
	if err := page.WaitLoad(); err != nil {
		t.Fatalf("fixture 未加载完成: %v", err)
	}
	return page
}

// drain 取回并清空事件缓冲，只保留目标元素上的事件。
func drain(t *testing.T, page *rod.Page, target string) []typedEvent {
	t.Helper()

	raw := page.MustEval(`() => { const e = window.__ev; window.__ev = []; return JSON.stringify(e) }`).Str()

	var evs []typedEvent
	if err := json.Unmarshal([]byte(raw), &evs); err != nil {
		t.Fatalf("解析事件失败: %v", err)
	}

	out := evs[:0]
	for _, e := range evs {
		if e.Target == target {
			out = append(out, e)
		}
	}
	return out
}

func resetField(t *testing.T, page *rod.Page, sel string) {
	t.Helper()

	page.MustEval(`(s) => {
		const e = document.querySelector(s);
		if (e.value !== undefined) { e.value = '' } else { e.textContent = '' }
		window.__ev = [];
	}`, sel)
}

func fieldText(t *testing.T, page *rod.Page, sel string) string {
	t.Helper()

	got := page.MustEval(`(s) => {
		const e = document.querySelector(s);
		return e.value !== undefined ? e.value : e.textContent;
	}`, sel).Str()

	// contenteditable 里行尾空格会变成 &nbsp;，那是浏览器行为不是输入错误。
	return strings.ReplaceAll(got, "\u00a0", " ")
}

type expectation struct {
	ascii   int // ASCII 可打印字符数
	cjk     int // 走 IME 的字符数
	literal int // 走 insertText 的字素簇数（emoji 等）
}

func expect(text string) expectation {
	var e expectation
	runes := []rune(text)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r >= 0x20 && r <= 0x7E:
			e.ascii++
			i++
		case unicode.Is(unicode.Han, r) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF01 && r <= 0xFF60):
			e.cjk++
			i++
		default:
			e.literal++
			i += clusterLen(runes[i:])
		}
	}
	return e
}

func tallyTypes(evs []typedEvent) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.type_()]++
	}
	return m
}

func (e typedEvent) type_() string {
	switch e.Type {
	case "keydown", "keyup":
		switch {
		case e.KeyCode == 229:
			return e.Type + ":ime"
		case e.Key == "Shift":
			return e.Type + ":shift"
		default:
			return e.Type + ":key"
		}
	case "input", "beforeinput":
		return e.Type + ":" + e.InputType
	}
	return e.Type
}

func checkStream(t *testing.T, label string, evs []typedEvent, want expectation) {
	t.Helper()

	c := tallyTypes(evs)

	// 1. ASCII：每个字符一对 keydown/keyup，一次 keypress，一次 insertText。
	if c["keydown:key"] != want.ascii {
		t.Errorf("%s: ASCII keydown %d 次，期望 %d 次", label, c["keydown:key"], want.ascii)
	}
	if c["keyup:key"] != want.ascii {
		t.Errorf("%s: ASCII keyup %d 次，期望 %d 次", label, c["keyup:key"], want.ascii)
	}
	if c["keypress"] != want.ascii {
		t.Errorf("%s: keypress %d 次，期望 %d 次", label, c["keypress"], want.ascii)
	}

	// 2. insertText 型 input：ASCII 字符 + emoji 字素簇，一个不多一个不少。
	//    多一次就是重复插入。
	wantInsert := want.ascii + want.literal
	if c["input:insertText"] != wantInsert {
		t.Errorf("%s: insertText 型 input %d 次，期望 %d 次（多出即重复插入）",
			label, c["input:insertText"], wantInsert)
	}
	if c["beforeinput:insertText"] != wantInsert {
		t.Errorf("%s: insertText 型 beforeinput %d 次，期望 %d 次", label, c["beforeinput:insertText"], wantInsert)
	}

	if want.cjk == 0 {
		if c["compositionstart"] != 0 {
			t.Errorf("%s: 无中文却有 %d 次 compositionstart", label, c["compositionstart"])
		}
		return
	}

	// 3. 每个提交块一次 compositionstart / compositionend。块长 1-4 字，随机。
	starts, ends := c["compositionstart"], c["compositionend"]
	if starts != ends {
		t.Errorf("%s: compositionstart %d 次，compositionend %d 次，不配对", label, starts, ends)
	}
	minChunks := (want.cjk + 3) / 4
	if starts < minChunks || starts > want.cjk {
		t.Errorf("%s: %d 个中文字分成 %d 个提交块，超出 [%d,%d]", label, want.cjk, starts, minChunks, want.cjk)
	}

	// 4. 每个字一次候选更新，每次提交再一次 —— 提交没有额外插入一遍。
	wantUpdates := want.cjk + starts
	if c["compositionupdate"] != wantUpdates {
		t.Errorf("%s: compositionupdate %d 次，期望 %d 次", label, c["compositionupdate"], wantUpdates)
	}
	if c["input:insertCompositionText"] != wantUpdates {
		t.Errorf("%s: insertCompositionText 型 input %d 次，期望 %d 次（多出即重复插入）",
			label, c["input:insertCompositionText"], wantUpdates)
	}

	// 5. 每次 compositionstart 之前必须有 keyCode 229 的按键。
	pending := 0
	for _, e := range evs {
		switch {
		case e.Type == "keydown" && e.KeyCode == 229:
			pending++
		case e.Type == "compositionstart":
			if pending < 2 {
				t.Errorf("%s: compositionstart 前只有 %d 个 229 按键，输入法不会凭空起字", label, pending)
			}
			pending = 0
		}
	}
	if c["keydown:ime"] != c["keyup:ime"] {
		t.Errorf("%s: 229 keydown %d 次、keyup %d 次，未配对", label, c["keydown:ime"], c["keyup:ime"])
	}
	if c["keydown:ime"] < want.cjk*2 {
		t.Errorf("%s: %d 个中文字只有 %d 次 229 按键，拼音敲得太少", label, want.cjk, c["keydown:ime"])
	}
}

func TestTypeEventStream(t *testing.T) {
	bin, err := browser.EnsureBrowser()
	if err != nil {
		t.Skipf("SKIP: 浏览器不可用: %v", err)
	}

	u := launcher.New().Bin(bin).Headless(true).MustLaunch()
	b := rod.New().ControlURL(u).MustConnect()
	defer b.MustClose()

	page := openTypeFixture(t, b)

	targets := []struct{ name, sel, id string }{
		{"input", "#a", "a"},
		{"textarea", "#t", "t"},
		{"contenteditable", "#b", "b"},
	}
	texts := []string{
		"你好abc😀",
		"测试中文 hello 😀 #标签",
	}

	for _, tg := range targets {
		for _, text := range texts {
			t.Run(fmt.Sprintf("%s/%s", tg.name, text), func(t *testing.T) {
				resetField(t, page, tg.sel)

				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				elem := page.MustElement(tg.sel)
				if err := Type(ctx, elem, text); err != nil {
					t.Fatalf("输入失败: %v", err)
				}

				if got := fieldText(t, page, tg.sel); got != text {
					t.Errorf("文本不符:\n 期望 %q\n 实际 %q", text, got)
				}

				evs := drain(t, page, tg.id)
				checkStream(t, tg.name, evs, expect(text))
				t.Logf("%s %q -> %d 个事件: %v", tg.name, text, len(evs), tallyTypes(evs))
			})
		}
	}
}

// 空闲时不应有任何残留事件：229 按键若带 NativeVirtualKeyCode，
// 这台浏览器会陷入每秒几千次的自动重复。
func TestTypeNoAutoRepeatStorm(t *testing.T) {
	bin, err := browser.EnsureBrowser()
	if err != nil {
		t.Skipf("SKIP: 浏览器不可用: %v", err)
	}

	u := launcher.New().Bin(bin).Headless(true).MustLaunch()
	b := rod.New().ControlURL(u).MustConnect()
	defer b.MustClose()

	page := openTypeFixture(t, b)
	resetField(t, page, "#a")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := Type(ctx, page.MustElement("#a"), "你好abc"); err != nil {
		t.Fatalf("输入失败: %v", err)
	}
	drain(t, page, "a")

	time.Sleep(1500 * time.Millisecond)

	if evs := drain(t, page, "a"); len(evs) != 0 {
		t.Fatalf("输入结束后 1.5 秒内仍有 %d 个事件，键位卡住了: %v", len(evs), tallyTypes(evs))
	}
}

// ctx 取消必须立刻中断输入。
func TestTypeContextCancel(t *testing.T) {
	bin, err := browser.EnsureBrowser()
	if err != nil {
		t.Skipf("SKIP: 浏览器不可用: %v", err)
	}

	u := launcher.New().Bin(bin).Headless(true).MustLaunch()
	b := rod.New().ControlURL(u).MustConnect()
	defer b.MustClose()

	page := openTypeFixture(t, b)
	resetField(t, page, "#a")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = Type(ctx, page.MustElement("#a"), strings.Repeat("abcdefghij", 20))
	if err == nil {
		t.Fatal("ctx 超时后 Type 应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ctx 取消后 %v 才返回，太慢", elapsed)
	}
	t.Logf("ctx 取消后返回: %v", err)
}
