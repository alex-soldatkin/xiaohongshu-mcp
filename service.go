package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/downloader"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/pacing"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/xhsutil"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

// XiaohongshuService 小红书业务服务
type XiaohongshuService struct {
	logins loginSessions

	// gate serialises browser work and enforces the activity budgets.
	// Every method that touches a browser goes through it via s.run.
	gate *pacing.Gate

	// browser owns the single long-lived browser and its persistent profile
	// (issue #6). It is the only place a browser is launched.
	browser *browser.Manager
}

// NewXiaohongshuService 创建小红书服务实例
func NewXiaohongshuService() *XiaohongshuService {
	profileDir := configs.ProfileDir()
	logrus.Infof("browser profile directory: %s", profileDir)

	gate := pacing.New(pacing.ConfigFromEnv())

	// Risk-control detection (issue #11) lives in the xiaohongshu package,
	// which has no access to the gate. Hand it the one thing it needs: the
	// ability to silence the gate when a challenge is seen twice.
	xiaohongshu.SetRiskCooldownHook(gate.Cooldown)

	return &XiaohongshuService{
		gate: gate,
		browser: browser.NewManager(browser.ManagerConfig{
			Headless: configs.IsHeadless(),
			Options: []browser.Option{
				browser.WithFingerprintSeed(configs.FingerprintSeed()),
				browser.WithProxy(configs.Proxy()),
				browser.WithTimezone(configs.Timezone()),
			},
			ProfileDir: profileDir,
			Session:    cookies.NewLoadCookie(cookies.GetCookiesFilePath()),
			Lifecycle:  configs.BrowserLifecycleFromEnv(),
		}),
	}
}

// Close releases everything the service owns: the pending login scan, if any,
// and the browser. Called from AppServer.Start after the HTTP server stops.
func (s *XiaohongshuService) Close(ctx context.Context) {
	s.logins.cancelCurrent()
	s.browser.Shutdown(ctx)
}

// Gate exposes the pacing gate, so risk-control detection (issue #11) can trip
// a cooldown from outside the service.
func (s *XiaohongshuService) Gate() *pacing.Gate {
	return s.gate
}

// run is the single path to a browser page.
//
// It takes the pacing slot for the given class, leases a page from the shared
// browser, hands it to fn and releases everything afterwards. Every service
// method below goes through it, so no method can accidentally skip the gate.
// Errors from the gate (*errors.ErrRateLimited) are returned to the caller
// unchanged.
func (s *XiaohongshuService) run(ctx context.Context, class pacing.Class, fn func(page *rod.Page) error) error {
	release, err := s.gate.Acquire(ctx, class)
	if err != nil {
		return err
	}
	defer release()

	lease, err := s.browser.Lease(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()

	return fn(lease.Page)
}

// PublishRequest 发布请求
type PublishRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Images     []string `json:"images" binding:"required,min=1"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	IsOriginal bool     `json:"is_original,omitempty"` // 是否声明原创
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// LoginStatusResponse 登录状态响应
type LoginStatusResponse struct {
	IsLoggedIn bool   `json:"is_logged_in"`
	Username   string `json:"username,omitempty"` // 当前登录账号的昵称
	UserID     string `json:"user_id,omitempty"`  // 用户唯一标识（个人主页 URL 中的 ID）
}

// LoginQrcodeResponse 登录扫码二维码
type LoginQrcodeResponse struct {
	Timeout    string `json:"timeout"`
	IsLoggedIn bool   `json:"is_logged_in"`
	Img        string `json:"img,omitempty"`
}

// PublishResponse 发布响应
type PublishResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Images  int    `json:"images"`
	Status  string `json:"status"`
}

// PublishVideoRequest 发布视频请求（仅支持本地单个视频文件）
type PublishVideoRequest struct {
	Title      string   `json:"title" binding:"required"`
	Content    string   `json:"content" binding:"required"`
	Video      string   `json:"video" binding:"required"`
	Tags       []string `json:"tags,omitempty"`
	ScheduleAt string   `json:"schedule_at,omitempty"` // 定时发布时间，ISO8601格式，为空则立即发布
	Visibility string   `json:"visibility,omitempty"`  // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products   []string `json:"products,omitempty"`    // 商品关键词列表，用于绑定带货商品
}

// PublishVideoResponse 发布视频响应
type PublishVideoResponse struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Video   string `json:"video"`
	Status  string `json:"status"`
}

// FeedsListResponse Feeds列表响应
type FeedsListResponse struct {
	Feeds []xiaohongshu.Feed `json:"feeds"`
	Count int                `json:"count"`
}

// UserProfileResponse 用户主页响应
type UserProfileResponse struct {
	UserBasicInfo xiaohongshu.UserBasicInfo      `json:"userBasicInfo"`
	Interactions  []xiaohongshu.UserInteractions `json:"interactions"`
	Feeds         []xiaohongshu.Feed             `json:"feeds"`
}

// resetTimeout bounds how long "reset login" waits for an in-flight action to
// finish before closing the browser underneath it.
const resetTimeout = 30 * time.Second

// DeleteCookies 删除 cookies 文件，用于登录重置。
//
// Deleting the file is no longer enough: the profile holds the live session, so
// it has to go too, and the browser holding it open has to be closed first.
//
// The fingerprint seed is written straight back. The account logs out and logs
// in again on the same device, which is what a human does; minting a new
// fingerprint at the same moment as a new login would be the suspicious
// version. The running process keeps the seed it already has, so the file and
// the process agree either way.
func (s *XiaohongshuService) DeleteCookies(ctx context.Context) error {
	store := cookies.NewLoadCookie(cookies.GetCookiesFilePath())

	// The seed the process is actually running with wins over the file's copy:
	// with XHS_FP_SEED set they can differ, and the file must describe the
	// device the next login will present.
	seed := configs.FingerprintSeed()
	if seed <= 0 {
		seed = store.LoadSeed()
	}

	if err := store.DeleteCookies(); err != nil {
		return err
	}

	resetCtx, cancel := context.WithTimeout(ctx, resetTimeout)
	defer cancel()
	if err := s.browser.Reset(resetCtx); err != nil {
		return err
	}

	if seed > 0 {
		if err := store.SaveSeed(seed); err != nil {
			// The account can still log in; it would just come back on a new
			// fingerprint after the next restart.
			logrus.Warnf("重置登录后保存 seed 失败: %v", err)
		}
	}
	return nil
}

// CheckLoginStatus 检查登录状态
func (s *XiaohongshuService) CheckLoginStatus(ctx context.Context) (*LoginStatusResponse, error) {
	response := &LoginStatusResponse{}

	err := s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		loginAction := xiaohongshu.NewLogin(page)

		isLoggedIn, err := loginAction.CheckLoginStatus(ctx)
		if err != nil {
			return err
		}
		response.IsLoggedIn = isLoggedIn

		// 已登录时从当前页读取真实账号信息；读不到只记 warn，不影响状态返回。
		if isLoggedIn {
			if user, err := loginAction.CurrentUser(ctx); err != nil {
				logrus.Warnf("failed to get current user info: %v", err)
			} else {
				response.Username = user.Nickname
				response.UserID = user.UserID
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return response, nil
}

// GetLoginQrcode 获取登录的扫码二维码
func (s *XiaohongshuService) GetLoginQrcode(ctx context.Context) (*LoginQrcodeResponse, error) {
	// Pre-empt the pending scan session before queueing for the gate. That
	// session holds the single-flight slot for up to 4 minutes and is only
	// cancelled by a replacement request, so acquiring first would leave this
	// call waiting on a slot that only this call could free.
	s.logins.cancelCurrent()

	// The gate slot is held for the whole scan wait, not just for fetching the
	// image: the browser has to stay alive to detect the scan, and a second
	// browser must never be launched alongside it.
	release, err := s.gate.Acquire(ctx, pacing.ClassRead)
	if err != nil {
		return nil, err
	}

	// The lease keeps the browser alive for the whole scan wait, which also
	// stops the idle timer and defers any due recycle until the scan is over.
	lease, err := s.browser.Lease(ctx)
	if err != nil {
		release()
		return nil, err
	}
	page := lease.Page

	var once sync.Once
	deferFunc := func() {
		once.Do(func() {
			lease.Release()
			release()
		})
	}

	loginAction := xiaohongshu.NewLogin(page)

	img, loggedIn, err := loginAction.FetchQrcodeImage(ctx)
	if err != nil || loggedIn {
		defer deferFunc()
	}
	if err != nil {
		return nil, err
	}

	timeout := 4 * time.Minute

	if !loggedIn {
		s.waitScanInBackground(loginAction, page, deferFunc, timeout)
	}

	return &LoginQrcodeResponse{
		Timeout: func() string {
			if loggedIn {
				return "0s"
			}
			return timeout.String()
		}(),
		Img:        img,
		IsLoggedIn: loggedIn,
	}, nil
}

// waitScanInBackground 在后台等用户扫码，扫上了就存 cookie。
//
// 浏览器必须一直活着才检测得到扫码，所以这里不能提前关；但也不能任由它堆积——
// 再取一次二维码就会把上一个还在等的会话关掉，同一时刻只留一个。
func (s *XiaohongshuService) waitScanInBackground(
	loginAction *xiaohongshu.LoginAction, page *rod.Page, closeBrowser func(), timeout time.Duration,
) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), timeout)
	seq := s.logins.start(cancel)
	logrus.Infof("等待扫码登录，会话 #%d，超时 %s", seq, timeout)

	go func() {
		defer closeBrowser()
		defer cancel()
		defer s.logins.finish(seq)

		if loginAction.WaitForLogin(ctxTimeout) {
			// Export explicitly while the page is still leased: the browser has
			// to be alive to read its cookie jar.
			if err := s.browser.ExportCookies(); err != nil {
				logrus.Errorf("扫码成功但保存 cookies 失败，会话 #%d: %v", seq, err)
				return
			}
			logrus.Infof("扫码登录成功，cookies 已保存，会话 #%d", seq)
			return
		}

		// 没等到扫码：要么超时，要么被新取的二维码取代
		logrus.Infof("登录会话 #%d 结束，未检测到扫码（超时或已被新的二维码取代）", seq)
	}()
}

// PublishContent 发布内容
func (s *XiaohongshuService) PublishContent(ctx context.Context, req *PublishRequest) (*PublishResponse, error) {
	// 验证标题长度（小红书限制：最大20个字）
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return nil, fmt.Errorf("标题长度超过限制")
	}

	imagePaths, err := s.processImages(req.Images)
	if err != nil {
		return nil, err
	}

	var scheduleTime *time.Time
	if req.ScheduleAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return nil, fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %v", err)
		}

		// 校验定时发布时间范围：1小时至14天
		now := time.Now()
		minTime := now.Add(1 * time.Hour)
		maxTime := now.Add(14 * 24 * time.Hour)

		if t.Before(minTime) {
			return nil, fmt.Errorf("定时发布时间必须至少在1小时后，当前设置: %s，最早可选: %s",
				t.Format("2006-01-02 15:04"), minTime.Format("2006-01-02 15:04"))
		}
		if t.After(maxTime) {
			return nil, fmt.Errorf("定时发布时间不能超过14天，当前设置: %s，最晚可选: %s",
				t.Format("2006-01-02 15:04"), maxTime.Format("2006-01-02 15:04"))
		}

		scheduleTime = &t
		logrus.Infof("设置定时发布时间: %s", t.Format("2006-01-02 15:04"))
	}

	content := xiaohongshu.PublishImageContent{
		Title:        req.Title,
		Content:      req.Content,
		Tags:         req.Tags,
		ImagePaths:   imagePaths,
		ScheduleTime: scheduleTime,
		IsOriginal:   req.IsOriginal,
		Visibility:   req.Visibility,
		Products:     req.Products,
	}

	if err := s.publishContent(ctx, content); err != nil {
		logrus.Errorf("发布内容失败: title=%s %v", content.Title, err)
		return nil, err
	}

	response := &PublishResponse{
		Title:   req.Title,
		Content: req.Content,
		Images:  len(imagePaths),
		Status:  "发布完成",
	}

	return response, nil
}

// processImages 处理图片列表，支持URL下载和本地路径
func (s *XiaohongshuService) processImages(images []string) ([]string, error) {
	processor := downloader.NewImageProcessor()
	return processor.ProcessImages(images)
}

// publishContent 执行内容发布
func (s *XiaohongshuService) publishContent(ctx context.Context, content xiaohongshu.PublishImageContent) error {
	return s.run(ctx, pacing.ClassPublish, func(page *rod.Page) error {
		action, err := xiaohongshu.NewPublishImageAction(page)
		if err != nil {
			return err
		}

		return action.Publish(ctx, content)
	})
}

// PublishVideo 发布视频（本地文件）
func (s *XiaohongshuService) PublishVideo(ctx context.Context, req *PublishVideoRequest) (*PublishVideoResponse, error) {
	// 标题长度校验（小红书限制：最大20个字）
	if xhsutil.CalcTitleLength(req.Title) > 20 {
		return nil, fmt.Errorf("标题长度超过限制")
	}

	// 本地视频文件校验
	if req.Video == "" {
		return nil, fmt.Errorf("必须提供本地视频文件")
	}
	if _, err := os.Stat(req.Video); err != nil {
		return nil, fmt.Errorf("视频文件不存在或不可访问: %v", err)
	}

	var scheduleTime *time.Time
	if req.ScheduleAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduleAt)
		if err != nil {
			return nil, fmt.Errorf("定时发布时间格式错误，请使用 ISO8601 格式: %v", err)
		}

		// 校验定时发布时间范围：1小时至14天
		now := time.Now()
		minTime := now.Add(1 * time.Hour)
		maxTime := now.Add(14 * 24 * time.Hour)

		if t.Before(minTime) {
			return nil, fmt.Errorf("定时发布时间必须至少在1小时后，当前设置: %s，最早可选: %s",
				t.Format("2006-01-02 15:04"), minTime.Format("2006-01-02 15:04"))
		}
		if t.After(maxTime) {
			return nil, fmt.Errorf("定时发布时间不能超过14天，当前设置: %s，最晚可选: %s",
				t.Format("2006-01-02 15:04"), maxTime.Format("2006-01-02 15:04"))
		}

		scheduleTime = &t
		logrus.Infof("设置定时发布时间: %s", t.Format("2006-01-02 15:04"))
	}

	content := xiaohongshu.PublishVideoContent{
		Title:        req.Title,
		Content:      req.Content,
		Tags:         req.Tags,
		VideoPath:    req.Video,
		ScheduleTime: scheduleTime,
		Visibility:   req.Visibility,
		Products:     req.Products,
	}

	if err := s.publishVideo(ctx, content); err != nil {
		return nil, err
	}

	resp := &PublishVideoResponse{
		Title:   req.Title,
		Content: req.Content,
		Video:   req.Video,
		Status:  "发布完成",
	}
	return resp, nil
}

// publishVideo 执行视频发布
func (s *XiaohongshuService) publishVideo(ctx context.Context, content xiaohongshu.PublishVideoContent) error {
	return s.run(ctx, pacing.ClassPublish, func(page *rod.Page) error {
		action, err := xiaohongshu.NewPublishVideoAction(page)
		if err != nil {
			return err
		}

		return action.PublishVideo(ctx, content)
	})
}

// ListFeeds 获取Feeds列表
func (s *XiaohongshuService) ListFeeds(ctx context.Context) (*FeedsListResponse, error) {
	var feeds []xiaohongshu.Feed

	err := s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		feeds, err = xiaohongshu.NewFeedsListAction(page).GetFeedsList(ctx)
		return err
	})
	if err != nil {
		logrus.Errorf("获取 Feeds 列表失败: %v", err)
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

func (s *XiaohongshuService) SearchFeeds(ctx context.Context, keyword string, filters ...xiaohongshu.FilterOption) (*FeedsListResponse, error) {
	var feeds []xiaohongshu.Feed

	err := s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		feeds, err = xiaohongshu.NewSearchAction(page).Search(ctx, keyword, filters...)
		return err
	})
	if err != nil {
		return nil, err
	}

	response := &FeedsListResponse{
		Feeds: feeds,
		Count: len(feeds),
	}

	return response, nil
}

// GetFeedDetail 获取Feed详情
func (s *XiaohongshuService) GetFeedDetail(ctx context.Context, feedID, xsecToken string, loadAllComments bool) (*FeedDetailResponse, error) {
	return s.GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, xiaohongshu.DefaultCommentLoadConfig())
}

// GetFeedDetailWithConfig 使用配置获取Feed详情
func (s *XiaohongshuService) GetFeedDetailWithConfig(ctx context.Context, feedID, xsecToken string, loadAllComments bool, config xiaohongshu.CommentLoadConfig) (*FeedDetailResponse, error) {
	var result *xiaohongshu.FeedDetailResponse

	err := s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewFeedDetailAction(page).GetFeedDetailWithConfig(ctx, feedID, xsecToken, loadAllComments, config)
		return err
	})
	if err != nil {
		return nil, err
	}

	response := &FeedDetailResponse{
		FeedID: feedID,
		Data:   result,
	}

	return response, nil
}

// UserProfile 获取用户信息
func (s *XiaohongshuService) UserProfile(ctx context.Context, userID, xsecToken, tab string) (*UserProfileResponse, error) {
	parsed, err := xiaohongshu.ParseProfileTab(tab)
	if err != nil {
		return nil, err
	}

	var result *xiaohongshu.UserProfileResponse

	err = s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewUserProfileAction(page).UserProfile(ctx, userID, xsecToken, parsed)
		return err
	})
	if err != nil {
		return nil, err
	}
	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil

}

// PostCommentToFeed 发表评论到Feed
func (s *XiaohongshuService) PostCommentToFeed(ctx context.Context, feedID, xsecToken, content string) (*PostCommentResponse, error) {
	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		return xiaohongshu.NewCommentFeedAction(page).PostComment(ctx, feedID, xsecToken, content)
	})
	if err != nil {
		return nil, err
	}

	return &PostCommentResponse{FeedID: feedID, Success: true, Message: "评论发表成功"}, nil
}

// LikeFeed 点赞笔记
func (s *XiaohongshuService) LikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		return xiaohongshu.NewLikeAction(page).Like(ctx, feedID, xsecToken)
	})
	if err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "点赞成功或已点赞"}, nil
}

// UnlikeFeed 取消点赞笔记
func (s *XiaohongshuService) UnlikeFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		return xiaohongshu.NewLikeAction(page).Unlike(ctx, feedID, xsecToken)
	})
	if err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "取消点赞成功或未点赞"}, nil
}

// FavoriteFeed 收藏笔记
func (s *XiaohongshuService) FavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		return xiaohongshu.NewFavoriteAction(page).Favorite(ctx, feedID, xsecToken)
	})
	if err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "收藏成功或已收藏"}, nil
}

// UnfavoriteFeed 取消收藏笔记
func (s *XiaohongshuService) UnfavoriteFeed(ctx context.Context, feedID, xsecToken string) (*ActionResult, error) {
	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		return xiaohongshu.NewFavoriteAction(page).Unfavorite(ctx, feedID, xsecToken)
	})
	if err != nil {
		return nil, err
	}
	return &ActionResult{FeedID: feedID, Success: true, Message: "取消收藏成功或未收藏"}, nil
}

// ReplyCommentToFeed 回复指定评论
func (s *XiaohongshuService) ReplyCommentToFeed(ctx context.Context, feedID, xsecToken, commentID, userID, content string) (*ReplyCommentResponse, error) {
	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		return xiaohongshu.NewCommentFeedAction(page).ReplyToComment(ctx, feedID, xsecToken, commentID, userID, content)
	})
	if err != nil {
		return nil, err
	}

	return &ReplyCommentResponse{
		FeedID:          feedID,
		TargetCommentID: commentID,
		TargetUserID:    userID,
		Success:         true,
		Message:         "评论回复成功",
	}, nil
}

// GetUnreadCount 获取通知未读数
func (s *XiaohongshuService) GetUnreadCount(ctx context.Context) (*xiaohongshu.NotificationCount, error) {
	var result *xiaohongshu.NotificationCount

	err := s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewNotificationAction(page).UnreadCount(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ListNotifications 获取指定分区的通知列表
func (s *XiaohongshuService) ListNotifications(ctx context.Context, tab string, limit int) (*xiaohongshu.NotificationList, error) {
	parsed, err := xiaohongshu.ParseNotificationTab(tab)
	if err != nil {
		return nil, err
	}

	var result *xiaohongshu.NotificationList

	err = s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewNotificationAction(page).List(ctx, parsed, limit)
		return err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// LikeNotification 给通知里的评论点赞或取消点赞
func (s *XiaohongshuService) LikeNotification(ctx context.Context, commentID string, unlike bool) (*xiaohongshu.NotificationLikeResult, error) {
	var result *xiaohongshu.NotificationLikeResult

	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewNotificationAction(page).Like(ctx, commentID, unlike)
		return err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ReplyNotification 在通知页就地回复评论
func (s *XiaohongshuService) ReplyNotification(ctx context.Context, commentID, content string) (*xiaohongshu.NotificationReplyResult, error) {
	var result *xiaohongshu.NotificationReplyResult

	err := s.run(ctx, pacing.ClassWrite, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewNotificationAction(page).Reply(ctx, commentID, content)
		return err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// GetMyProfile 获取当前登录用户的个人信息
func (s *XiaohongshuService) GetMyProfile(ctx context.Context, tab string) (*UserProfileResponse, error) {
	parsed, err := xiaohongshu.ParseProfileTab(tab)
	if err != nil {
		return nil, err
	}

	var result *xiaohongshu.UserProfileResponse

	err = s.run(ctx, pacing.ClassRead, func(page *rod.Page) error {
		var err error
		result, err = xiaohongshu.NewUserProfileAction(page).GetMyProfileViaSidebar(ctx, parsed)
		return err
	})

	if err != nil {
		return nil, err
	}

	response := &UserProfileResponse{
		UserBasicInfo: result.UserBasicInfo,
		Interactions:  result.Interactions,
		Feeds:         result.Feeds,
	}

	return response, nil
}
