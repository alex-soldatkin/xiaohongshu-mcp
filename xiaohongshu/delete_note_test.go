package xiaohongshu

import (
	"strings"
	"testing"
)

// The exact payload the live rednote note card carries, HTML-unescaped. The id
// is the only part of the card that names the note, so this parser is the
// whole of the card-to-note mapping.
const liveImpression = `{"index":{"type":"Index","value":{"channelTabName":"all"}},` +
	`"noteTarget":{"type":"NoteTarget","value":{"noteId":"695acc29000000001e02799d"}},` +
	`"event":{"type":"Event","value":{"action":{"type":"NormalizedAction","value":"impression"},"pointId":50977}},` +
	`"page":{"type":"Page","value":{"pageInstance":{"type":"PageInstance","value":"creator_service_platform"}}}}`

func TestParseNoteCardImpression(t *testing.T) {
	id, err := parseNoteCardImpression(liveImpression)
	if err != nil {
		t.Fatalf("live payload did not parse: %v", err)
	}
	if id != "695acc29000000001e02799d" {
		t.Fatalf("note id = %q", id)
	}
}

// Anything that does not name a note must fail rather than return an empty id:
// an empty id would match a card whose attribute is also unreadable, and the
// caller would then delete whichever card happened to come first.
func TestParseNoteCardImpressionRejectsNonNotes(t *testing.T) {
	cases := map[string]string{
		"not json":        "[object Object]",
		"no noteTarget":   `{"index":{"type":"Index","value":{"channelTabName":"all"}}}`,
		"empty note id":   `{"noteTarget":{"type":"NoteTarget","value":{"noteId":""}}}`,
		"empty attribute": "",
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if id, err := parseNoteCardImpression(raw); err == nil {
				t.Fatalf("accepted %q as note id %q", raw, id)
			}
		})
	}
}

// The confirmation labels are tried in order, and the one that names the action
// must win over the generic one: a dialog offering both 删除 and 取消 must not
// be answered by matching a prefix.
func TestDeleteConfirmLabelOrder(t *testing.T) {
	if deleteConfirmLabels[0] != "确认删除" {
		t.Fatalf("most specific label is not first: %v", deleteConfirmLabels)
	}
	for _, label := range deleteConfirmLabels {
		if strings.Contains(label, "取消") {
			t.Fatalf("a cancel label is in the confirm set: %v", deleteConfirmLabels)
		}
	}
}

func TestNotesManagerURL(t *testing.T) {
	if got := SiteRednote.NotesManagerURL(); got != "https://creator.rednote.com/new/note-manager" {
		t.Fatalf("rednote notes manager URL = %q", got)
	}
	if got := SiteXiaohongshu.NotesManagerURL(); got != "https://creator.xiaohongshu.com/new/note-manager" {
		t.Fatalf("xiaohongshu notes manager URL = %q", got)
	}
}

func TestContainsID(t *testing.T) {
	ids := []string{"a", "b"}
	if !containsID(ids, "b") {
		t.Fatal("containsID missed a present id")
	}
	if containsID(ids, "c") {
		t.Fatal("containsID matched an absent id")
	}
	if containsID(nil, "a") {
		t.Fatal("containsID matched against an empty list")
	}
}
