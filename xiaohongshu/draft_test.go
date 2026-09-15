package xiaohongshu

import (
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

func TestParseDraftCount(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		want   int
		wantOK bool
	}{
		{"ascii brackets", "草稿箱(3)", 3, true},
		{"fullwidth brackets", "草稿箱（12）", 12, true},
		{"spaced", "草稿箱 (0)", 0, true},
		{"no number is a readable zero", "草稿箱", 0, true},
		{"leading text", "笔记管理 草稿箱(7)", 7, true},
		// A label that continues into other words carries no count; reading it
		// as zero would make a later non-zero read look like a success.
		{"word without count", "草稿箱里没有内容", 0, false},
		{"unrelated", "发布笔记", 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseDraftCount(c.text)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("parseDraftCount(%q) = (%d, %v), want (%d, %v)", c.text, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// The XPath must match an element's own text node rather than its subtree, so
// that the innermost label is found instead of an ancestor that contains it.
func TestDraftButtonXPath(t *testing.T) {
	xpath := draftButtonXPath()

	if !strings.Contains(xpath, "normalize-space(text())='暂存离开'") {
		t.Fatalf("XPath does not match the live rednote label 暂存离开: %s", xpath)
	}
	if strings.Contains(xpath, "normalize-space(.)") {
		t.Fatalf("XPath matches the subtree, which would select an ancestor: %s", xpath)
	}
	for _, label := range draftButtonLabels {
		if !strings.Contains(xpath, label) {
			t.Fatalf("label %q missing from XPath %s", label, xpath)
		}
	}
}

// The draft button on the live rednote footer sits in a closed shadow root, so
// the search must recurse through ShadowRoots as well as Children, and must
// match a button's own text rather than its subtree.
func TestFindButtonNodeWalksShadowRoots(t *testing.T) {
	text := func(s string) *proto.DOMNode { return &proto.DOMNode{NodeType: 3, NodeValue: s} }
	button := func(label string, backendID int) *proto.DOMNode {
		return &proto.DOMNode{
			NodeName:      "BUTTON",
			BackendNodeID: proto.DOMBackendNodeID(backendID),
			Children:      []*proto.DOMNode{text(label)},
		}
	}

	// The shape measured live: host > #document-fragment (closed) >
	// div[data-v-app] > div.publish-page-publish-btn > two buttons.
	host := &proto.DOMNode{
		NodeName: "XHS-PUBLISH-BTN",
		ShadowRoots: []*proto.DOMNode{{
			NodeName: "#document-fragment",
			Children: []*proto.DOMNode{{
				NodeName: "DIV",
				Children: []*proto.DOMNode{{
					NodeName: "DIV",
					Children: []*proto.DOMNode{button("暂存离开", 11), button("发布", 22)},
				}},
			}},
		}},
	}

	match := func(s string) bool { return s == "暂存离开" }
	got := findButtonNode(host, match)
	if got == nil {
		t.Fatal("draft button not found inside the shadow root")
	}
	if got.BackendNodeID != 11 {
		t.Fatalf("found the wrong button: backendNodeID %d", got.BackendNodeID)
	}

	// A label nobody renders must not fall back to the publish button.
	if n := findButtonNode(host, func(s string) bool { return s == "不存在" }); n != nil {
		t.Fatalf("matched an unexpected button: %q", nodeOwnText(n))
	}

	// Only a direct text child is the label: an ancestor that merely contains
	// the text must not match.
	wrapper := &proto.DOMNode{
		NodeName: "BUTTON",
		Children: []*proto.DOMNode{{NodeName: "SPAN", Children: []*proto.DOMNode{text("暂存离开")}}},
	}
	if nodeOwnText(wrapper) != "" {
		t.Fatalf("nodeOwnText read a descendant's text: %q", nodeOwnText(wrapper))
	}
}
