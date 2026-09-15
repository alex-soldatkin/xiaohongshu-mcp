//go:build integration

// 集成测试：起浏览器 + 本地 HTTP 服务，默认 go test 不编译不运行。
// 手动跑：GOARCH=arm64 go test -tags integration ./xiaohongshu/ -run TestReadState
package xiaohongshu

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
)

// stateFixtureData is the logical content of __INITIAL_STATE__, written once as
// plain JSON. The fixture page then installs it in three different shapes, so
// every shape carries byte-identical logical content and any difference the
// reader reports is a difference in ref handling, not in the data.
const stateFixtureData = `{
  "feed": {
    "feeds": [
      {
        "id": "feed-1",
        "xsecToken": "TOKEN-1",
        "modelType": "note",
        "index": 0,
        "noteCard": {
          "type": "normal",
          "displayTitle": "第一篇",
          "user": {"userId": "u-1", "nickname": "甲", "nickName": "甲", "avatar": "https://img/1"},
          "interactInfo": {"liked": true, "likedCount": "12", "commentCount": "3", "collected": false, "collectedCount": "1", "sharedCount": "0"}
        }
      },
      {
        "id": "feed-2",
        "xsecToken": "TOKEN-2",
        "modelType": "note",
        "index": 1,
        "noteCard": {
          "type": "video",
          "displayTitle": "第二篇",
          "user": {"userId": "u-2", "nickname": "乙", "nickName": "乙", "avatar": "https://img/2"},
          "interactInfo": {"liked": false, "likedCount": "0", "commentCount": "0", "collected": true, "collectedCount": "7", "sharedCount": "2"}
        }
      },
      {"id": "live-1", "xsecToken": "", "modelType": "live_v2", "index": 2}
    ]
  },
  "search": {
    "feeds": [
      {"id": "s-1", "xsecToken": "S-1", "modelType": "note", "index": 0, "noteCard": {"type": "normal", "displayTitle": "搜到的"}},
      {"id": "s-2", "xsecToken": "S-2", "modelType": "note", "index": 1, "noteCard": {"type": "normal", "displayTitle": "也搜到了"}}
    ]
  },
  "note": {
    "noteDetailMap": {
      "feed-1": {
        "note": {
          "noteId": "feed-1",
          "xsecToken": "TOKEN-1",
          "title": "第一篇",
          "desc": "正文",
          "type": "normal",
          "time": 1700000000000,
          "ipLocation": "上海",
          "user": {"userId": "u-1", "nickname": "甲", "nickName": "甲", "avatar": "https://img/1"},
          "interactInfo": {"liked": true, "likedCount": "12", "commentCount": "3", "collected": false, "collectedCount": "1", "sharedCount": "0"}
        },
        "comments": {"list": [], "cursor": "", "hasMore": false}
      }
    }
  },
  "notification": {
    "notificationCount": {"mentions": 1, "likes": 2, "connections": 3, "unreadCount": 6},
    "notificationMap": {
      "mentions": {
        "hasMore": true,
        "messageList": [
          {
            "id": "n-1",
            "type": "comment/like",
            "title": "赞了你的评论",
            "time": 1700000001000,
            "userInfo": {"userid": "u-9", "nickname": "丙", "xsecToken": "U-9"},
            "commentInfo": {"id": "c-1", "content": "好看", "liked": true, "illegalInfo": {"illegalStatus": "NORMAL"}},
            "itemInfo": {"id": "feed-1", "type": "note_info", "content": "第一篇", "xsecToken": "TOKEN-1", "illegalInfo": {"illegalStatus": "NORMAL"}}
          }
        ]
      },
      "likes": {"hasMore": false, "messageList": []},
      "connections": {"hasMore": false, "messageList": []}
    }
  },
  "user": {
    "userInfo": {"guest": false, "nickname": "我", "userId": "me-1"},
    "userPageData": {
      "basicInfo": {"gender": 1, "ipLocation": "上海", "desc": "签名", "nickname": "我", "redId": "circle", "images": "https://img/me", "imageb": "https://img/meb"},
      "interactions": [
        {"type": "follows", "name": "关注", "count": "10"},
        {"type": "fans", "name": "粉丝", "count": "20"}
      ]
    },
    "notes": [
      [{"id": "p-0", "xsecToken": "P-0", "modelType": "note", "index": 0, "noteCard": {"type": "normal", "displayTitle": "笔记 tab"}}],
      [{"id": "p-1", "xsecToken": "P-1", "modelType": "note", "index": 0, "noteCard": {"type": "normal", "displayTitle": "收藏 tab"}}]
    ],
    "activeTab": {"index": 1, "query": "fav"}
  }
}`

// stateFixtureHTML installs stateFixtureData under one of three shapes.
//
//   - plain:      no refs anywhere, the shape the reader must stay a no-op on;
//   - serialized: the SSR form, plain objects carrying _rawValue/_value;
//   - live:       Vue's RefImpl, a `.value` getter plus a circular `dep`.
//
// Every object and array node is wrapped, so refs sit several hops deep and a
// ref's value contains further refs — the case a single leaf-level unwrap
// (which is what the old readers did) gets wrong.
const stateFixtureHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>state fixture</title></head>
<body><p id="ok">ok</p>
<script>
const DATA = %s;

// The SSR payload: plain data, no getter.
function serializedRef(v) {
  return {__v_isRef: true, __v_isShallow: false, _rawValue: v, _value: v};
}

// A stand-in for Vue's RefImpl. The subscriber list in dep is deliberately
// circular and points back at the ref, which is exactly what makes a naive
// JSON.stringify over a ref subtree throw.
class RefImpl {
  constructor(v) {
    this.__v_isRef = true;
    this.__v_isShallow = false;
    this._rawValue = v;
    this._value = v;
    const dep = {computed: null, subs: null};
    const link = {dep: dep, sub: this, prevSub: null, nextSub: null};
    link.prevSub = link;
    link.nextSub = link;
    dep.subs = link;
    dep.owner = this;
    this.dep = dep;
  }
  get value() { return this._value; }
}

function deepWrap(v, mk) {
  if (v === null || typeof v !== 'object') return v;
  if (Array.isArray(v)) return mk(v.map(function (x) { return deepWrap(x, mk); }));
  const out = {};
  for (const k of Object.keys(v)) out[k] = deepWrap(v[k], mk);
  return mk(out);
}

const SHAPES = {
  plain: function (v) { return v; },
  serialized: serializedRef,
  live: function (v) { return new RefImpl(v); }
};

window.__INITIAL_STATE__ = deepWrap(DATA, SHAPES[%q]);
</script></body></html>`

// stateFixtureShapes are the three installations, in the order the tests report them.
var stateFixtureShapes = []string{"plain", "serialized", "live"}

// newStateFixture serves the fixture page at /<shape> for each shape.
func newStateFixture(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		shape := req.URL.Path[1:]
		valid := false
		for _, s := range stateFixtureShapes {
			if s == shape {
				valid = true
			}
		}
		if !valid {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, stateFixtureHTML, stateFixtureData, shape)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// openShape 打开某一种形态的 fixture 页。
func openShape(t *testing.T, b *headless_browser.Browser, srv *httptest.Server, shape string) *rod.Page {
	t.Helper()

	page := b.NewPage()
	t.Cleanup(func() { _ = page.Close() })
	page = page.Timeout(30 * time.Second)
	require.NoError(t, page.Navigate(srv.URL+"/"+shape))
	require.NoError(t, page.WaitLoad())
	return page
}

// TestReadStateAcrossShapes is the acceptance gate for the unwrap helper: the
// same logical state, installed three different ways, must read back identically.
func TestReadStateAcrossShapes(t *testing.T) {
	b := browser.NewBrowser(true)
	defer b.Close()

	srv := newStateFixture(t)

	paths := []string{
		"feed.feeds",
		"search.feeds",
		"note.noteDetailMap",
		"note.noteDetailMap.feed-1.note.interactInfo",
		"notification.notificationCount",
		"notification.notificationMap.mentions",
		"user.userInfo",
		"user.userPageData",
		"user.notes",
		"user.activeTab",
	}

	got := make(map[string]map[string]string, len(stateFixtureShapes))
	for _, shape := range stateFixtureShapes {
		page := openShape(t, b, srv, shape)
		got[shape] = make(map[string]string, len(paths))
		for _, path := range paths {
			raw, err := readStateJSON(page, path)
			require.NoErrorf(t, err, "shape %s, path %s", shape, path)
			require.NotEmptyf(t, raw, "shape %s: path %s read back empty", shape, path)
			got[shape][path] = raw
		}
	}

	// Every shape must agree with the plain one — that is the whole claim.
	for _, path := range paths {
		for _, shape := range stateFixtureShapes[1:] {
			assert.JSONEqf(t, got["plain"][path], got[shape][path],
				"shape %s disagrees with plain at %s", shape, path)
		}
	}
}

// TestReadStateLiveRefsWouldBreakNaiveStringify pins the reason the replacer
// exists. A live ref's dep is circular, so stringifying a subtree that contains
// one throws; the helper must never walk dep and must succeed on the same page.
func TestReadStateLiveRefsWouldBreakNaiveStringify(t *testing.T) {
	b := browser.NewBrowser(true)
	defer b.Close()

	srv := newStateFixture(t)
	page := openShape(t, b, srv, "live")

	res, err := page.Eval(`() => {
		try {
			JSON.stringify(window.__INITIAL_STATE__.value.note);
			return "no-throw";
		} catch (e) {
			return "throw:" + e.name;
		}
	}`)
	require.NoError(t, err)
	require.Contains(t, res.Value.Str(), "throw",
		"fixture is not exercising the circular dep — a naive stringify should have thrown")

	raw, err := readStateJSON(page, "note.noteDetailMap")
	require.NoError(t, err)
	require.NotEmpty(t, raw, "the helper must read the same subtree the naive stringify choked on")
}

// TestReadStateMissingPath: 路径不存在是干净的「没有」，不是错误。
func TestReadStateMissingPath(t *testing.T) {
	b := browser.NewBrowser(true)
	defer b.Close()

	srv := newStateFixture(t)

	missing := []string{
		"nope",
		"feed.nope",
		"feed.feeds.nope",
		"note.noteDetailMap.no-such-note",
		"note.noteDetailMap.feed-1.note.nope.deeper",
		"overseasLogin.status",
	}

	for _, shape := range stateFixtureShapes {
		page := openShape(t, b, srv, shape)
		for _, path := range missing {
			raw, err := readStateJSON(page, path)
			assert.NoErrorf(t, err, "shape %s, path %s: a missing path must not be an error", shape, path)
			assert.Emptyf(t, raw, "shape %s, path %s: expected empty result", shape, path)
		}
	}
}

// TestReadStateUnmarshalsRealTypes 读出来的 JSON 要能喂给生产代码用的那些结构体。
func TestReadStateUnmarshalsRealTypes(t *testing.T) {
	b := browser.NewBrowser(true)
	defer b.Close()

	srv := newStateFixture(t)

	for _, shape := range stateFixtureShapes {
		t.Run(shape, func(t *testing.T) {
			page := openShape(t, b, srv, shape)

			var feeds []Feed
			ok, err := readState(page, "feed.feeds", &feeds)
			require.NoError(t, err)
			require.True(t, ok)
			require.Len(t, feeds, 3)
			assert.Equal(t, "feed-1", feeds[0].ID)
			assert.Equal(t, "TOKEN-1", feeds[0].XsecToken)
			assert.Equal(t, "甲", feeds[0].NoteCard.User.Nickname)
			assert.Equal(t, "12", feeds[0].NoteCard.InteractInfo.LikedCount)
			// live_v2 条目要被 onlyNotes 滤掉，说明 modelType 也完整穿过来了。
			assert.Len(t, onlyNotes(feeds), 2)

			var count rawCount
			ok, err = readState(page, "notification.notificationCount", &count)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, rawCount{Mentions: 1, Likes: 2, Connections: 3, Unread: 6}, count)

			var payload notificationPayload
			ok, err = readState(page, "notification.notificationMap."+string(TabMentions), &payload)
			require.NoError(t, err)
			require.True(t, ok)
			assert.True(t, payload.HasMore)
			require.Len(t, payload.MessageList, 1)
			assert.Equal(t, "n-1", payload.MessageList[0].ID)
			assert.Equal(t, "丙", payload.MessageList[0].from().Nickname)
			assert.Equal(t, "feed-1", payload.MessageList[0].Item.ID)
			assert.Equal(t, itemTypeNote, payload.MessageList[0].Item.Type)
			assert.True(t, payload.MessageList[0].visible())

			// feed_detail.go 的 noteDetailMap 结构，原样照抄。
			var noteDetailMap map[string]struct {
				Note     FeedDetail  `json:"note"`
				Comments CommentList `json:"comments"`
			}
			ok, err = readState(page, "note.noteDetailMap", &noteDetailMap)
			require.NoError(t, err)
			require.True(t, ok)
			detail, exists := noteDetailMap["feed-1"]
			require.True(t, exists)
			assert.Equal(t, "第一篇", detail.Note.Title)
			assert.Equal(t, "上海", detail.Note.IPLocation)
			assert.True(t, detail.Note.InteractInfo.Liked)
			assert.False(t, detail.Comments.HasMore)

			var notes [][]Feed
			ok, err = readState(page, "user.notes", &notes)
			require.NoError(t, err)
			require.True(t, ok)
			require.Len(t, notes, 2)
			assert.Equal(t, "p-1", notes[1][0].ID)

			var activeTab struct {
				Index int    `json:"index"`
				Query string `json:"query"`
			}
			ok, err = readState(page, "user.activeTab", &activeTab)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, 1, activeTab.Index)
			assert.Equal(t, "fav", activeTab.Query)
		})
	}
}

// TestWaitState: 已经在页面上的路径立刻返回，不存在的路径按超时报错而不是干等。
func TestWaitState(t *testing.T) {
	b := browser.NewBrowser(true)
	defer b.Close()

	srv := newStateFixture(t)
	page := openShape(t, b, srv, "live")

	ctx := context.Background()
	require.NoError(t, waitState(ctx, page, "user.userPageData", 5*time.Second))

	start := time.Now()
	err := waitState(ctx, page, "user.neverAppears", 600*time.Millisecond)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "waitState overran its timeout")

	// ctx 取消要立刻返回，不等满 timeout。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, waitState(cancelled, page, "user.neverAppears", time.Minute), context.Canceled)
}
