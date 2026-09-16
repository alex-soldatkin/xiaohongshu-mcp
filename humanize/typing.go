package humanize

import (
	"context"
	"math/rand"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// Typing emits a realistic keyboard event stream instead of bare Input.insertText.
//
// Three paths, picked per character run:
//
//   - ASCII printable: real keyDown/keyUp (plus a held Shift for shifted
//     characters), so the page sees keydown/keypress/beforeinput/input/keyup.
//   - CJK: IME simulation. Pinyin keystrokes go out as keyCode 229 "Process"
//     keys carrying no text, the candidate is built with Input.imeSetComposition
//     (real compositionstart/compositionupdate events), and the chunk is
//     committed with Input.insertText, which ends the composition.
//   - Everything else (emoji, newlines): Input.insertText per grapheme cluster.
//     People do enter emoji from a picker or the clipboard, so the absence of
//     keystrokes is correct there.
//
// Two facts about the bundled Chromium build drive the implementation and were
// measured rather than assumed:
//
//  1. Input.imeSetComposition is available and fires genuine composition events,
//     so the keyCode 229 pairs do not have to fake them.
//  2. Setting NativeVirtualKeyCode on a dispatched key event triggers a runaway
//     auto-repeat: thousands of keydown events per second with keyCode 0 and key
//     "Unidentified", which no keyUp stops. Every key event below therefore
//     leaves NativeVirtualKeyCode unset. rod's own Key.Encode does the same.
const imeVirtualKeyCode = 229

// Plausible physical keys for pinyin input. Only the Code field of the
// keystroke varies; the key itself always reports as "Process"/229, which is
// exactly what Chrome reports while an IME is composing.
var pinyinKeyCodes = []string{
	"KeyA", "KeyB", "KeyC", "KeyD", "KeyE", "KeyF", "KeyG", "KeyH", "KeyI",
	"KeyJ", "KeyK", "KeyL", "KeyM", "KeyN", "KeyO", "KeyP", "KeyQ", "KeyR",
	"KeyS", "KeyT", "KeyU", "KeyW", "KeyX", "KeyY", "KeyZ",
}

// Shifted ASCII characters that are reached with the Shift key on a US layout.
const shiftedPunctuation = "~!@#$%^&*()_+{}|:\"<>?"

type segmentKind int

const (
	// Real key events.
	segASCII segmentKind = iota
	// IME composition followed by a commit.
	segIME
	// Input.insertText per grapheme cluster.
	segLiteral
)

type segment struct {
	kind segmentKind
	text []rune
}

func classify(r rune) segmentKind {
	switch {
	case r >= 0x20 && r <= 0x7E:
		return segASCII
	case isIMEChar(r):
		return segIME
	default:
		return segLiteral
	}
}

// isIMEChar reports whether a character is normally produced by a Chinese IME.
// Han characters plus the CJK punctuation and halfwidth/fullwidth blocks, which
// a pinyin IME emits for 。，！？ and friends.
func isIMEChar(r rune) bool {
	if unicode.Is(unicode.Han, r) {
		return true
	}
	if r >= 0x3000 && r <= 0x303F {
		return true
	}
	if r >= 0xFF01 && r <= 0xFF60 {
		return true
	}
	return false
}

func segmentText(text string) []segment {
	var segs []segment
	for _, r := range text {
		k := classify(r)
		if n := len(segs); n > 0 && segs[n-1].kind == k {
			segs[n-1].text = append(segs[n-1].text, r)
			continue
		}
		segs = append(segs, segment{kind: k, text: []rune{r}})
	}
	return segs
}

// pause waits for d, or returns early if ctx is cancelled.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func keystrokeDelay() time.Duration {
	return defaultProvider.Timing()[Keystroke].Sample()
}

// keyInfoFor looks up a rune in rod's keymap. Key.Info panics on unknown keys,
// so unknown runes fall back to the literal path.
func keyInfoFor(r rune) (info input.KeyInfo, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return input.Key(r).Info(), true
}

func needsShift(r rune) bool {
	if r >= 'A' && r <= 'Z' {
		return true
	}
	for _, s := range shiftedPunctuation {
		if r == s {
			return true
		}
	}
	return false
}

// typeASCII sends one keyDown/keyUp pair per character, holding Shift across a
// run of shifted characters the way a hand does.
func typeASCII(ctx context.Context, page *rod.Page, runes []rune) error {
	shiftHeld := false
	shiftInfo := input.ShiftLeft.Info()

	defer func() {
		if shiftHeld {
			_ = proto.InputDispatchKeyEvent{
				Type:                  proto.InputDispatchKeyEventTypeKeyUp,
				Key:                   shiftInfo.Key,
				Code:                  shiftInfo.Code,
				WindowsVirtualKeyCode: shiftInfo.KeyCode,
			}.Call(page)
		}
	}()

	for _, r := range runes {
		if err := ctx.Err(); err != nil {
			return err
		}

		info, ok := keyInfoFor(r)
		if !ok {
			if err := insertCluster(ctx, page, string(r)); err != nil {
				return err
			}
			continue
		}

		want := needsShift(r)
		if want != shiftHeld {
			if err := setShift(ctx, page, &shiftHeld, want); err != nil {
				return err
			}
		}

		modifiers := 0
		if shiftHeld {
			modifiers = input.ModifierShift
		}

		base := proto.InputDispatchKeyEvent{
			Key:                   info.Key,
			Code:                  info.Code,
			WindowsVirtualKeyCode: info.KeyCode,
			Modifiers:             modifiers,
		}

		down := base
		down.Type = proto.InputDispatchKeyEventTypeKeyDown
		// Text is what makes the character actually appear; it must be set on
		// keyDown only, never combined with a separate insertText for the same
		// character, or the character is inserted twice.
		down.Text = info.Key
		down.UnmodifiedText = info.Key
		if err := down.Call(page); err != nil {
			return err
		}

		if err := pause(ctx, defaultProvider.Timing()[ClickHold].Sample()/3); err != nil {
			return err
		}

		up := base
		up.Type = proto.InputDispatchKeyEventTypeKeyUp
		if err := up.Call(page); err != nil {
			return err
		}

		d := keystrokeDelay()
		// Pause longer after punctuation and spaces, the way a person does.
		if r == ' ' || r == ',' || r == '.' || r == '?' || r == '!' {
			d += keystrokeDelay()
		}
		if err := pause(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func setShift(ctx context.Context, page *rod.Page, held *bool, want bool) error {
	if want == *held {
		return nil
	}

	info := input.ShiftLeft.Info()
	ev := proto.InputDispatchKeyEvent{
		Type:                  proto.InputDispatchKeyEventTypeKeyUp,
		Key:                   info.Key,
		Code:                  info.Code,
		WindowsVirtualKeyCode: info.KeyCode,
	}
	if want {
		ev.Type = proto.InputDispatchKeyEventTypeRawKeyDown
		ev.Modifiers = input.ModifierShift
	}
	if err := ev.Call(page); err != nil {
		return err
	}

	*held = want
	return pause(ctx, keystrokeDelay()/2)
}

// processKey sends a keyCode 229 "Process" pair: what Chrome reports for every
// key pressed while an IME is composing. It carries no text, so it inserts
// nothing; the text arrives through imeSetComposition and insertText.
func processKey(ctx context.Context, page *rod.Page, code string) error {
	ev := proto.InputDispatchKeyEvent{
		Type:                  proto.InputDispatchKeyEventTypeKeyDown,
		Key:                   "Process",
		Code:                  code,
		WindowsVirtualKeyCode: imeVirtualKeyCode,
	}
	if err := ev.Call(page); err != nil {
		return err
	}

	if err := pause(ctx, defaultProvider.Timing()[ClickHold].Sample()/3); err != nil {
		return err
	}

	ev.Type = proto.InputDispatchKeyEventTypeKeyUp
	if err := ev.Call(page); err != nil {
		return err
	}
	return pause(ctx, keystrokeDelay())
}

// imeChunkSize splits a CJK run into 1-4 character "words", the unit a pinyin
// user actually commits.
func imeChunkSize(remaining int) int {
	n := 1 + rand.Intn(4)
	if n > remaining {
		n = remaining
	}
	return n
}

// typeIME composes and commits a run of CJK characters.
//
// The composition preview carries the Chinese characters rather than raw pinyin
// because we have no pinyin table. Inline-candidate IMEs (Sogou, Microsoft
// Pinyin) show exactly this, and it keeps the field from briefly holding Latin
// text that the page could snapshot.
func typeIME(ctx context.Context, page *rod.Page, runes []rune) error {
	for len(runes) > 0 {
		n := imeChunkSize(len(runes))
		chunk := runes[:n]
		runes = runes[n:]

		if err := composeChunk(ctx, page, chunk); err != nil {
			return err
		}
	}
	return nil
}

func composeChunk(ctx context.Context, page *rod.Page, chunk []rune) error {
	composing := false

	for i := range chunk {
		if err := ctx.Err(); err != nil {
			return err
		}

		// 2-4 pinyin keystrokes per character, then the candidate grows by one.
		for k := 2 + rand.Intn(3); k > 0; k-- {
			if err := processKey(ctx, page, pinyinKeyCodes[rand.Intn(len(pinyinKeyCodes))]); err != nil {
				return err
			}
		}

		preview := string(chunk[:i+1])
		err := proto.InputImeSetComposition{
			Text:           preview,
			SelectionStart: i + 1,
			SelectionEnd:   i + 1,
		}.Call(page)
		if err != nil {
			// Composition unavailable: fall back to a plain commit rather than
			// leaving the chunk half-entered.
			if composing {
				_ = proto.InputImeSetComposition{Text: "", SelectionStart: 0, SelectionEnd: 0}.Call(page)
			}
			return page.InsertText(string(chunk))
		}
		composing = true
	}

	// The candidate-selection key (space on most IMEs) also reports as 229.
	if err := processKey(ctx, page, "Space"); err != nil {
		return err
	}

	// insertText replaces the composition and ends it: one beforeinput/input
	// pair with inputType insertCompositionText, then compositionend. No second
	// insertion, because the composition range is what gets replaced.
	if err := page.InsertText(string(chunk)); err != nil {
		return err
	}
	return pause(ctx, keystrokeDelay())
}

// typeLiteral inserts characters that have no keyboard of their own, one
// grapheme cluster at a time so emoji sequences stay intact.
func typeLiteral(ctx context.Context, page *rod.Page, runes []rune) error {
	for len(runes) > 0 {
		n := clusterLen(runes)
		if err := insertCluster(ctx, page, string(runes[:n])); err != nil {
			return err
		}
		runes = runes[n:]
	}
	return nil
}

func insertCluster(ctx context.Context, page *rod.Page, s string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := page.InsertText(s); err != nil {
		return err
	}
	return pause(ctx, keystrokeDelay())
}

// clusterLen returns the length in runes of the grapheme cluster starting at
// runes[0]. Enough of UAX #29 to keep emoji whole: ZWJ sequences, variation
// selectors, skin tone modifiers, keycaps, flags and combining marks.
func clusterLen(runes []rune) int {
	if len(runes) == 0 {
		return 0
	}

	if isRegionalIndicator(runes[0]) && len(runes) > 1 && isRegionalIndicator(runes[1]) {
		return 2
	}

	n := 1
	for n < len(runes) {
		r := runes[n]
		if r == 0x200D { // zero width joiner: glue on whatever follows
			n++
			if n < len(runes) {
				n++
			}
			continue
		}
		if isExtender(r) {
			n++
			continue
		}
		break
	}
	return n
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

func isExtender(r rune) bool {
	switch {
	case r >= 0xFE00 && r <= 0xFE0F: // variation selectors
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tone modifiers
		return true
	case r == 0x20E3: // combining enclosing keycap
		return true
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return true
	}
	return false
}

// Type enters text into elem as a human would: real key events for ASCII, an
// IME composition for Chinese, and insertText for emoji. Callers pass the whole
// string; segmentation happens here.
func Type(ctx context.Context, elem *rod.Element, text string) error {
	if err := elem.Focus(); err != nil {
		return err
	}
	if err := elem.WaitEnabled(); err != nil {
		return err
	}
	if err := elem.WaitWritable(); err != nil {
		return err
	}

	page := elem.Page().Context(ctx)

	for _, seg := range segmentText(text) {
		if err := ctx.Err(); err != nil {
			return err
		}

		var err error
		switch seg.kind {
		case segASCII:
			err = typeASCII(ctx, page, seg.text)
		case segIME:
			err = typeIME(ctx, page, seg.text)
		default:
			err = typeLiteral(ctx, page, seg.text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
