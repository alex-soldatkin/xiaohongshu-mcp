package xiaohongshu

import (
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

// The publish button lives beside the draft button inside the same closed
// shadow root, so the pierced search must pick the one the widget's own
// submit-text names rather than whichever button comes first.
func TestFindButtonNodeSelectsSubmitButton(t *testing.T) {
	text := func(s string) *proto.DOMNode { return &proto.DOMNode{NodeType: 3, NodeValue: s} }
	button := func(label string, backendID int) *proto.DOMNode {
		return &proto.DOMNode{
			NodeName:      "BUTTON",
			BackendNodeID: proto.DOMBackendNodeID(backendID),
			Children:      []*proto.DOMNode{text(label)},
		}
	}

	// The shape measured live on rednote: the draft button comes first in
	// document order, so a search that ignores the label would click it.
	host := &proto.DOMNode{
		NodeName: "XHS-PUBLISH-BTN",
		ShadowRoots: []*proto.DOMNode{{
			NodeName: "#document-fragment",
			Children: []*proto.DOMNode{{
				NodeName: "DIV",
				Children: []*proto.DOMNode{button("暂存离开", 11), button("发布", 22)},
			}},
		}},
	}

	got := findButtonNode(host, func(s string) bool { return s == "发布" })
	if got == nil {
		t.Fatal("publish button not found inside the shadow root")
	}
	if got.BackendNodeID != 22 {
		t.Fatalf("found the wrong button: backendNodeID %d", got.BackendNodeID)
	}

	// With no submit-text to go on, the fallback labels must still find it and
	// must not settle for the draft button.
	matchAny := func(s string) bool {
		for _, l := range publishSubmitLabels {
			if s == l {
				return true
			}
		}
		return false
	}
	got = findButtonNode(host, matchAny)
	if got == nil || got.BackendNodeID != 22 {
		t.Fatalf("fallback labels did not select the publish button: %+v", got)
	}

	// A label the deployment does not render must find nothing rather than
	// fall through to the neighbouring button.
	if n := findButtonNode(host, func(s string) bool { return s == "存草稿" }); n != nil {
		t.Fatalf("matched an unexpected button: %q", nodeOwnText(n))
	}
}

// The draft and publish label sets must stay disjoint: one shared match
// function walking the same tree would otherwise be able to click 暂存离开
// when it was asked for 发布.
func TestPublishAndDraftLabelsAreDisjoint(t *testing.T) {
	for _, p := range publishSubmitLabels {
		for _, d := range draftButtonLabels {
			if p == d {
				t.Fatalf("label %q is both a publish and a draft label", p)
			}
		}
	}
}
