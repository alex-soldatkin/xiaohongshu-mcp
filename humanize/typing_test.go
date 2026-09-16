package humanize

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSegmentText(t *testing.T) {
	segs := segmentText("你好abc😀")

	if assert.Len(t, segs, 3) {
		assert.Equal(t, segIME, segs[0].kind)
		assert.Equal(t, "你好", string(segs[0].text))
		assert.Equal(t, segASCII, segs[1].kind)
		assert.Equal(t, "abc", string(segs[1].text))
		assert.Equal(t, segLiteral, segs[2].kind)
		assert.Equal(t, "😀", string(segs[2].text))
	}
}

func TestSegmentTextRoundTrip(t *testing.T) {
	for _, text := range []string{
		"你好abc😀",
		"测试中文 hello 😀 #标签",
		"",
		"\n换行\n",
		"2024-01-02 15:04",
		"🇨🇳👨‍👩‍👧‍👦👍🏽",
	} {
		var got string
		for _, s := range segmentText(text) {
			got += string(s.text)
		}
		assert.Equal(t, text, got, "分段后必须能原样拼回")
	}
}

func TestSegmentNewlineIsLiteral(t *testing.T) {
	// Newlines still go through insertText: tiptap may treat Enter as a block
	// split.
	for _, s := range segmentText("a\nb") {
		if string(s.text) == "\n" {
			assert.Equal(t, segLiteral, s.kind)
			return
		}
	}
	t.Fatal("没有把换行单独分出来")
}

func TestClusterLen(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"😀", 1},
		{"👍🏽", 2},      // skin-tone modifier
		{"🇨🇳", 2},      // regional indicator pair
		{"👨‍👩‍👧‍👦", 7}, // ZWJ family
		{"❤️", 2},      // variation selector
		{"\n", 1},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, clusterLen([]rune(c.text)), "字素簇 %q", c.text)
	}
	assert.Equal(t, 0, clusterLen(nil))
}

func TestNeedsShift(t *testing.T) {
	for _, r := range []rune{'A', 'Z', '#', '!', '?', '_'} {
		assert.True(t, needsShift(r), "%q 需要 Shift", r)
	}
	for _, r := range []rune{'a', 'z', '1', '-', ' ', ','} {
		assert.False(t, needsShift(r), "%q 不需要 Shift", r)
	}
}

func TestKeyInfoForUnknownRune(t *testing.T) {
	// An undefined key must not panic; it can only fall back to insertText.
	_, ok := keyInfoFor('你')
	assert.False(t, ok)

	info, ok := keyInfoFor('#')
	if assert.True(t, ok) {
		assert.Equal(t, "Digit3", info.Code)
	}
}

func TestIMEChunkSize(t *testing.T) {
	for remaining := 1; remaining <= 10; remaining++ {
		for i := 0; i < 50; i++ {
			n := imeChunkSize(remaining)
			assert.GreaterOrEqual(t, n, 1)
			assert.LessOrEqual(t, n, remaining)
			assert.LessOrEqual(t, n, 4)
		}
	}
}
