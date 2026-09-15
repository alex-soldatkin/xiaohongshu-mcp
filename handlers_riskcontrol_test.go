package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

// A flagged account is not a server fault and not a client mistake, so it is
// neither a 500 nor the 429 the pacing gate returns: 423 Locked, with a code of
// its own, is what lets a caller tell "you have been challenged" from "this
// broke".
func TestRespondServiceError_RiskControlBecomes423(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/feeds/comment", nil)

	// Wrapped, to prove the mapping does not depend on the error being on top.
	err := fmt.Errorf("发表评论失败: %w", &myerrors.ErrRiskControl{
		Kind:       "captcha_dom",
		URL:        "https://www.xiaohongshu.com/explore/653f",
		Detail:     "matched captcha_dom(dom-container=\"captcha\")",
		Screenshot: "/tmp/xiaohongshu_images/risk_captcha_dom_20260915_120000.png",
		Cooldown:   30 * time.Minute,
	})

	respondServiceError(c, "POST_COMMENT_FAILED", "发表评论失败", err)

	if rec.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1800" {
		t.Fatalf("Retry-After = %q, want \"1800\"", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if body["code"] != "RISK_CONTROL" {
		t.Fatalf("code = %v, want RISK_CONTROL", body["code"])
	}

	details, ok := body["details"].(map[string]any)
	if !ok {
		t.Fatalf("details missing or not an object: %v", body["details"])
	}
	if details["kind"] != "captcha_dom" {
		t.Fatalf("details.kind = %v", details["kind"])
	}
	if details["screenshot"] == nil {
		t.Fatal("details.screenshot missing — the operator needs the evidence path")
	}
}

// The first signal reports without a cooldown, so there is no deadline to
// promise: no Retry-After, and the caller may try again immediately.
func TestRespondServiceError_RiskControlWithoutCooldownHasNoRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/feeds/list", nil)

	respondServiceError(c, "LIST_FEEDS_FAILED", "获取Feeds列表失败", &myerrors.ErrRiskControl{
		Kind: "login_required",
		URL:  "https://www.xiaohongshu.com/explore",
	})

	if rec.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("unexpected Retry-After: %q", got)
	}
}
