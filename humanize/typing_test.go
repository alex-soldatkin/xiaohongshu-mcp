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
	// 换行仍走 insertText：tiptap 可能把 Enter 当成分块。
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
		{"👍🏽", 2},      // 肤色修饰符
		{"🇨🇳", 2},      // 区域指示符对
		{"👨‍👩‍👧‍👦", 7}, // ZWJ 家庭
		{"❤️", 2},      // 变体选择符
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
	// 未定义的键不能 panic，只能落回 insertText。
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
