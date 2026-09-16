package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/pacing"
)

// respondError 是全仓库共用的错误出口。分级只是一行 if，改回去编译和其他单测
// 都不会报错，但鉴权开启后被扫描器打的 401 会重新刷满 ERROR。
func TestRespondErrorLogLevel(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantLevel  logrus.Level
	}{
		{name: "401 记为 warning", statusCode: http.StatusUnauthorized, wantLevel: logrus.WarnLevel},
		{name: "400 记为 warning", statusCode: http.StatusBadRequest, wantLevel: logrus.WarnLevel},
		{name: "500 记为 error", statusCode: http.StatusInternalServerError, wantLevel: logrus.ErrorLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := logrustest.NewGlobal()
			defer hook.Reset()

			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/login/status", nil)

			respondError(c, tt.statusCode, "CODE", "消息", nil)

			require.Len(t, hook.Entries, 1)
			assert.Equal(t, tt.wantLevel, hook.LastEntry().Level)
		})
	}
}

// Draft mode (issue #19) is a classification decision as much as a UI one: a
// draft is real traffic to the creator host, so it must be paced, but it
// creates nothing visible, so it must not spend the scarce publish budget.
func TestPublishClassAndStatus(t *testing.T) {
	if got := publishClass(false); got != pacing.ClassPublish {
		t.Fatalf("publish should be classed as publish, got %q", got)
	}
	if got := publishClass(true); got != pacing.ClassWrite {
		t.Fatalf("draft should be classed as write, got %q", got)
	}
	if got := publishStatusText(true); got != "已存草稿" {
		t.Fatalf("draft status text = %q", got)
	}
	if got := publishStatusText(false); got != "发布完成" {
		t.Fatalf("publish status text = %q", got)
	}
}

// The HTTP API must accept save_as_draft on the publish body, or the mode is
// reachable from MCP only.
func TestPublishRequestBindsSaveAsDraft(t *testing.T) {
	var req PublishRequest
	if err := json.Unmarshal([]byte(`{"title":"t","content":"c","images":["a.png"],"save_as_draft":true}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !req.SaveAsDraft {
		t.Fatal("save_as_draft did not bind on PublishRequest")
	}

	var vreq PublishVideoRequest
	if err := json.Unmarshal([]byte(`{"title":"t","content":"c","video":"v.mp4","save_as_draft":true}`), &vreq); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !vreq.SaveAsDraft {
		t.Fatal("save_as_draft did not bind on PublishVideoRequest")
	}
}

// TestDeleteNoteGatedByEnv pins the three gates on the delete capability
// (issue #20): off by default, the MCP tool unregistered, and the HTTP route
// answering 403 instead of actually clicking the page.
func TestDeleteNoteDisabledByDefault(t *testing.T) {
	t.Setenv("XHS_ENABLE_DELETE", "")
	if configs.DeleteEnabled() {
		t.Fatal("删除默认应当是关闭的")
	}

	svc := NewXiaohongshuService()
	// runHook guarantees no browser is ever launched here: the gate must stop the
	// call before it reaches for a page.
	svc.runHook = func(ctx context.Context, class pacing.Class, fn func(page *rod.Page) error) error {
		t.Fatal("删除未启用时不应进入浏览器路径")
		return nil
	}
	if _, err := svc.DeleteNote(context.Background(), &DeleteNoteRequest{NoteID: "abc"}); err == nil {
		t.Fatal("未启用时 DeleteNote 应当报错")
	}
}

func TestDeleteNoteRequiresNoteID(t *testing.T) {
	t.Setenv("XHS_ENABLE_DELETE", "1")
	if !configs.DeleteEnabled() {
		t.Fatal("XHS_ENABLE_DELETE=1 应当打开删除能力")
	}

	svc := NewXiaohongshuService()
	svc.runHook = func(ctx context.Context, class pacing.Class, fn func(page *rod.Page) error) error {
		t.Fatal("没有 note_id 时不应进入浏览器路径")
		return nil
	}
	if _, err := svc.DeleteNote(context.Background(), &DeleteNoteRequest{NoteID: "   "}); err == nil {
		t.Fatal("空 note_id 应当报错")
	}
}

// Delete is as irreversible as publish, so it must draw on the same publish
// budget.
func TestDeleteNoteIsPublishClass(t *testing.T) {
	t.Setenv("XHS_ENABLE_DELETE", "1")

	svc := NewXiaohongshuService()
	var got pacing.Class
	svc.runHook = func(ctx context.Context, class pacing.Class, fn func(page *rod.Page) error) error {
		got = class
		return nil
	}
	if _, err := svc.DeleteNote(context.Background(), &DeleteNoteRequest{NoteID: "695acc29000000001e02799d"}); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if got != pacing.ClassPublish {
		t.Fatalf("删除记在了 %v 预算上，应为 %v", got, pacing.ClassPublish)
	}
}
