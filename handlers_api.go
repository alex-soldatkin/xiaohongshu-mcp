package main

import (
	"math"
	"net/http"
	"strconv"

	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// respondError 返回错误响应
func respondError(c *gin.Context, statusCode int, code, message string, details any) {
	response := ErrorResponse{
		Error:   message,
		Code:    code,
		Details: details,
	}

	// 4xx 是调用方传错，5xx 才是服务端故障，用日志级别区分开。
	// 否则鉴权开启后被扫描器打，401 会把 ERROR 刷满。
	if statusCode < http.StatusInternalServerError {
		logrus.Warnf("%s %s %d", c.Request.Method, c.Request.URL.Path, statusCode)
	} else {
		logrus.Errorf("%s %s %d", c.Request.Method, c.Request.URL.Path, statusCode)
	}

	c.JSON(statusCode, response)
}

// respondServiceError maps a service-layer error to an HTTP response.
//
// The pacing gate refuses work with *errors.ErrRateLimited; that is a 429 with
// a Retry-After header, not a 500 — the caller did nothing wrong and should try
// again later. Everything else stays a 500 with the handler's own code.
func respondServiceError(c *gin.Context, code, message string, err error) {
	if rl, ok := myerrors.AsRateLimited(err); ok {
		retry := int(math.Ceil(rl.RetryAfter.Seconds()))
		if retry < 1 {
			retry = 1
		}
		c.Header("Retry-After", strconv.Itoa(retry))
		respondError(c, http.StatusTooManyRequests, "RATE_LIMITED",
			"操作被本地限流拦截，请稍后重试", map[string]any{
				"reason":      rl.Reason,
				"class":       rl.Class,
				"retry_after": retry,
			})
		return
	}

	// Risk control is a 423 Locked: the account, not the request, is the thing
	// that is unavailable. Distinct from the 429 above, which is our own local
	// budget and carries a deadline we actually know.
	if rc, ok := myerrors.AsRiskControl(err); ok {
		details := map[string]any{
			"kind":   rc.Kind,
			"url":    rc.URL,
			"detail": rc.Detail,
		}
		if rc.Screenshot != "" {
			details["screenshot"] = rc.Screenshot
		}
		if rc.Cooldown > 0 {
			retry := int(math.Ceil(rc.Cooldown.Seconds()))
			c.Header("Retry-After", strconv.Itoa(retry))
			details["retry_after"] = retry
		}
		respondError(c, http.StatusLocked, "RISK_CONTROL",
			"疑似触发小红书风控，已暂停该账号的操作", details)
		return
	}

	respondError(c, http.StatusInternalServerError, code, message, err.Error())
}

// respondSuccess 返回成功响应
func respondSuccess(c *gin.Context, data any, message string) {
	response := SuccessResponse{
		Success: true,
		Data:    data,
		Message: message,
	}

	logrus.Infof("%s %s %d", c.Request.Method, c.Request.URL.Path, http.StatusOK)

	c.JSON(http.StatusOK, response)
}

// checkLoginStatusHandler 检查登录状态
func (s *AppServer) checkLoginStatusHandler(c *gin.Context) {
	status, err := s.xiaohongshuService.CheckLoginStatus(c.Request.Context())
	if err != nil {
		respondServiceError(c, "STATUS_CHECK_FAILED",
			"检查登录状态失败", err)
		return
	}

	respondSuccess(c, status, "检查登录状态成功")
}

// getLoginQrcodeHandler 处理 [GET /api/v1/login/qrcode] 请求。
// 用于生成并返回登录二维码（Base64 图片 + 超时时间），供前端展示给用户扫码登录。
func (s *AppServer) getLoginQrcodeHandler(c *gin.Context) {
	result, err := s.xiaohongshuService.GetLoginQrcode(c.Request.Context())
	if err != nil {
		respondServiceError(c, "STATUS_CHECK_FAILED",
			"获取登录二维码失败", err)
		return
	}

	respondSuccess(c, result, "获取登录二维码成功")
}

// deleteCookiesHandler 删除 cookies，重置登录状态
func (s *AppServer) deleteCookiesHandler(c *gin.Context) {
	err := s.xiaohongshuService.DeleteCookies(c.Request.Context())
	if err != nil {
		respondServiceError(c, "DELETE_COOKIES_FAILED",
			"删除 cookies 失败", err)
		return
	}

	cookiePath := cookies.GetCookiesFilePath()
	respondSuccess(c, map[string]interface{}{
		"cookie_path": cookiePath,
		"message":     "Cookies 已成功删除，登录状态已重置。下次操作时需要重新登录。",
	}, "删除 cookies 成功")
}

// publishHandler 发布内容
func (s *AppServer) publishHandler(c *gin.Context) {
	var req PublishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.PublishContent(c.Request.Context(), &req)
	if err != nil {
		respondServiceError(c, "PUBLISH_FAILED",
			"发布失败", err)
		return
	}

	respondSuccess(c, result, "发布成功")
}

// publishVideoHandler 发布视频内容
func (s *AppServer) publishVideoHandler(c *gin.Context) {
	var req PublishVideoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.PublishVideo(c.Request.Context(), &req)
	if err != nil {
		respondServiceError(c, "PUBLISH_VIDEO_FAILED",
			"视频发布失败", err)
		return
	}

	respondSuccess(c, result, "视频发布成功")
}

// deleteNoteHandler 删除已发布笔记（issue #20）。
//
// 能力闸门在 service 层，这里只负责把它变成一个 403 而不是 500：没开启就是
// 没开启，跟参数错误区分开。
func (s *AppServer) deleteNoteHandler(c *gin.Context) {
	if !configs.DeleteEnabled() {
		respondError(c, http.StatusForbidden, "DELETE_DISABLED",
			"删除功能未启用", "需要设置环境变量 XHS_ENABLE_DELETE=1")
		return
	}

	var req DeleteNoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.DeleteNote(c.Request.Context(), &req)
	if err != nil {
		respondServiceError(c, "DELETE_NOTE_FAILED", "删除笔记失败", err)
		return
	}

	respondSuccess(c, result, "删除成功")
}

// listFeedsHandler 获取Feeds列表
func (s *AppServer) listFeedsHandler(c *gin.Context) {
	result, err := s.xiaohongshuService.ListFeeds(c.Request.Context())
	if err != nil {
		respondServiceError(c, "LIST_FEEDS_FAILED",
			"获取Feeds列表失败", err)
		return
	}

	respondSuccess(c, result, "获取Feeds列表成功")
}

// searchFeedsHandler 搜索Feeds
func (s *AppServer) searchFeedsHandler(c *gin.Context) {
	var keyword string
	var filters xiaohongshu.FilterOption

	switch c.Request.Method {
	case http.MethodPost:
		// 对于POST请求，从JSON中获取keyword
		var searchReq SearchFeedsRequest
		if err := c.ShouldBindJSON(&searchReq); err != nil {
			respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
				"请求参数错误", err.Error())
			return
		}
		keyword = searchReq.Keyword
		filters = searchReq.Filters
	default:
		keyword = c.Query("keyword")
	}

	if keyword == "" {
		respondError(c, http.StatusBadRequest, "MISSING_KEYWORD",
			"缺少关键词参数", "keyword parameter is required")
		return
	}

	result, err := s.xiaohongshuService.SearchFeeds(c.Request.Context(), keyword, filters)
	if err != nil {
		respondServiceError(c, "SEARCH_FEEDS_FAILED",
			"搜索Feeds失败", err)
		return
	}

	respondSuccess(c, result, "搜索Feeds成功")
}

// getFeedDetailHandler 获取Feed详情
func (s *AppServer) getFeedDetailHandler(c *gin.Context) {
	var req FeedDetailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	var result *FeedDetailResponse
	var err error

	if req.CommentConfig != nil {
		config := xiaohongshu.CommentLoadConfig{
			ClickMoreReplies:    req.CommentConfig.ClickMoreReplies,
			MaxRepliesThreshold: req.CommentConfig.MaxRepliesThreshold,
			MaxCommentItems:     req.CommentConfig.MaxCommentItems,
			ScrollSpeed:         req.CommentConfig.ScrollSpeed,
		}
		result, err = s.xiaohongshuService.GetFeedDetailWithConfig(c.Request.Context(), req.FeedID, req.XsecToken, req.LoadAllComments, config)
	} else {
		result, err = s.xiaohongshuService.GetFeedDetail(c.Request.Context(), req.FeedID, req.XsecToken, req.LoadAllComments)
	}

	if err != nil {
		respondServiceError(c, "GET_FEED_DETAIL_FAILED",
			"获取Feed详情失败", err)
		return
	}

	respondSuccess(c, result, "获取Feed详情成功")
}

// userProfileHandler 用户主页
func (s *AppServer) userProfileHandler(c *gin.Context) {
	var req UserProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.UserProfile(c.Request.Context(), req.UserID, req.XsecToken, req.Tab)
	if err != nil {
		respondServiceError(c, "GET_USER_PROFILE_FAILED",
			"获取用户主页失败", err)
		return
	}

	respondSuccess(c, map[string]any{"data": result}, "result.Message")
}

// postCommentHandler 发表评论到Feed
func (s *AppServer) postCommentHandler(c *gin.Context) {
	var req PostCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	// 发表评论
	result, err := s.xiaohongshuService.PostCommentToFeed(c.Request.Context(), req.FeedID, req.XsecToken, req.Content)
	if err != nil {
		respondServiceError(c, "POST_COMMENT_FAILED",
			"发表评论失败", err)
		return
	}

	respondSuccess(c, result, result.Message)
}

// replyCommentHandler 回复指定评论
func (s *AppServer) replyCommentHandler(c *gin.Context) {
	var req ReplyCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.ReplyCommentToFeed(c.Request.Context(), req.FeedID, req.XsecToken, req.CommentID, req.UserID, req.Content)
	if err != nil {
		respondServiceError(c, "REPLY_COMMENT_FAILED",
			"回复评论失败", err)
		return
	}

	respondSuccess(c, result, result.Message)
}

// likeFeedHandler 点赞/取消点赞
func (s *AppServer) likeFeedHandler(c *gin.Context) {
	var req LikeFeedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	var result *ActionResult
	var err error
	if req.Unlike {
		result, err = s.xiaohongshuService.UnlikeFeed(c.Request.Context(), req.FeedID, req.XsecToken)
	} else {
		result, err = s.xiaohongshuService.LikeFeed(c.Request.Context(), req.FeedID, req.XsecToken)
	}
	if err != nil {
		respondServiceError(c, "LIKE_FEED_FAILED",
			"点赞操作失败", err)
		return
	}

	respondSuccess(c, result, result.Message)
}

// favoriteFeedHandler 收藏/取消收藏
func (s *AppServer) favoriteFeedHandler(c *gin.Context) {
	var req FavoriteFeedRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	var result *ActionResult
	var err error
	if req.Unfavorite {
		result, err = s.xiaohongshuService.UnfavoriteFeed(c.Request.Context(), req.FeedID, req.XsecToken)
	} else {
		result, err = s.xiaohongshuService.FavoriteFeed(c.Request.Context(), req.FeedID, req.XsecToken)
	}
	if err != nil {
		respondServiceError(c, "FAVORITE_FEED_FAILED",
			"收藏操作失败", err)
		return
	}

	respondSuccess(c, result, result.Message)
}

// healthHandler 健康检查
func healthHandler(c *gin.Context) {
	respondSuccess(c, map[string]any{
		"status":    "healthy",
		"service":   "xiaohongshu-mcp",
		"version":   version,
		"account":   "github.com/xpzouying/xiaohongshu-mcp",
		"timestamp": "now",
	}, "服务正常")
}

// myProfileHandler 我的信息
func (s *AppServer) myProfileHandler(c *gin.Context) {
	// 获取当前登录用户信息
	result, err := s.xiaohongshuService.GetMyProfile(c.Request.Context(), c.Query("tab"))
	if err != nil {
		respondServiceError(c, "GET_MY_PROFILE_FAILED",
			"获取我的主页失败", err)
		return
	}

	respondSuccess(c, map[string]any{"data": result}, "获取我的主页成功")
}

// getUnreadCountHandler 获取通知未读数
func (s *AppServer) getUnreadCountHandler(c *gin.Context) {
	result, err := s.xiaohongshuService.GetUnreadCount(c.Request.Context())
	if err != nil {
		respondServiceError(c, "GET_UNREAD_COUNT_FAILED",
			"获取未读数失败", err)
		return
	}

	respondSuccess(c, map[string]any{"data": result}, "获取未读数成功")
}

// listNotificationsHandler 获取通知列表。两个参数都可选，因此 GET 走 query、POST 走 JSON。
func (s *AppServer) listNotificationsHandler(c *gin.Context) {
	var req ListNotificationsRequest

	if c.Request.Method == http.MethodPost {
		if err := c.ShouldBindJSON(&req); err != nil {
			respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
				"请求参数错误", err.Error())
			return
		}
	} else {
		req.Tab = c.Query("tab")
		if limit, err := strconv.Atoi(c.Query("limit")); err == nil {
			req.Limit = limit
		}
	}

	result, err := s.xiaohongshuService.ListNotifications(c.Request.Context(), req.Tab, req.Limit)
	if err != nil {
		respondServiceError(c, "LIST_NOTIFICATIONS_FAILED",
			"获取通知列表失败", err)
		return
	}

	respondSuccess(c, map[string]any{"data": result}, "获取通知列表成功")
}

// replyNotificationHandler 回复通知里的评论
func (s *AppServer) replyNotificationHandler(c *gin.Context) {
	var req ReplyNotificationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.ReplyNotification(c.Request.Context(), req.CommentID, req.Content)
	if err != nil {
		respondServiceError(c, "REPLY_NOTIFICATION_FAILED",
			"回复通知失败", err)
		return
	}

	respondSuccess(c, map[string]any{"data": result}, "回复成功")
}

// likeNotificationHandler 给通知里的评论点赞/取消点赞
func (s *AppServer) likeNotificationHandler(c *gin.Context) {
	var req LikeNotificationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "INVALID_REQUEST",
			"请求参数错误", err.Error())
		return
	}

	result, err := s.xiaohongshuService.LikeNotification(c.Request.Context(), req.CommentID, req.Unlike)
	if err != nil {
		respondServiceError(c, "LIKE_NOTIFICATION_FAILED",
			"点赞失败", err)
		return
	}

	respondSuccess(c, map[string]any{"data": result}, "操作成功")
}
