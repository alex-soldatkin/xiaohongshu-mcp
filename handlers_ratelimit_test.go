package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

func TestRespondServiceError_RateLimitedBecomes429(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/feeds/like", nil)

	// Wrapped, to prove the mapping does not depend on the error being at the top.
	err := fmt.Errorf("点赞操作失败: %w", &myerrors.ErrRateLimited{
		Class:      "write",
		Reason:     "writes budget exhausted (20/hour)",
		RetryAfter: 90 * time.Second,
	})

	respondServiceError(c, "LIKE_FEED_FAILED", "点赞操作失败", err)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want \"90\"", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if body["code"] != "RATE_LIMITED" {
		t.Fatalf("code = %v, want RATE_LIMITED", body["code"])
	}
}

func TestRespondServiceError_OtherErrorsStay500(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/feeds/list", nil)

	respondServiceError(c, "LIST_FEEDS_FAILED", "获取Feeds列表失败", errors.New("boom"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("unexpected Retry-After: %q", got)
	}
}
