package xiaohongshu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// ProfileTab 个人主页的子 tab。
type ProfileTab string

const (
	TabNotes     ProfileTab = "note"
	TabFavorites ProfileTab = "fav"
	TabLiked     ProfileTab = "liked"
)

// ParseProfileTab 解析 tab 名，空值默认为「笔记」。
func ParseProfileTab(s string) (ProfileTab, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "note", "notes", "笔记":
		return TabNotes, nil
	case "fav", "favorites", "favorite", "收藏":
		return TabFavorites, nil
	case "liked", "like", "点赞":
		return TabLiked, nil
	}
	return "", fmt.Errorf("未知的主页 tab %q，可选：note / fav / liked", s)
}

// tabLabel 子 tab 对应的页面文字。
var tabLabel = map[ProfileTab]string{
	TabNotes:     "笔记",
	TabFavorites: "收藏",
	TabLiked:     "点赞",
}

type UserProfileAction struct {
	page *rod.Page
}

func NewUserProfileAction(page *rod.Page) *UserProfileAction {
	pp := page.Timeout(60 * time.Second)
	return &UserProfileAction{page: pp}
}

// UserProfile 获取用户基本信息及指定 tab 下的帖子
func (u *UserProfileAction) UserProfile(ctx context.Context, userID, xsecToken string, tab ProfileTab) (*UserProfileResponse, error) {
	page := u.page.Context(ctx).Timeout(60 * time.Second) // 重设被 .Context 清掉的 deadline

	profileURL := makeUserProfileURL(userID, xsecToken, tab)
	// A profile is reached from the feed, where the author's name is a link.
	if err := navigateFrom(ctx, page, profileURL, urlExplore, navWaitStable); err != nil {
		return nil, err
	}

	return u.extractUserProfileData(ctx, page, tab)
}

// extractUserProfileData 从页面中提取用户资料数据的通用方法
func (u *UserProfileAction) extractUserProfileData(ctx context.Context, page *rod.Page, tab ProfileTab) (*UserProfileResponse, error) {
	// 等资料注水。原先等的是 __INITIAL_STATE__ 本身存在，而它从首屏起就在，等于没等。
	if err := waitState(ctx, page, "user.userPageData", 10*time.Second); err != nil {
		return nil, fmt.Errorf("user.userPageData not found in __INITIAL_STATE__: %w", err)
	}

	// 解析用户信息
	var userPageData struct {
		Interactions []UserInteractions `json:"interactions"`
		BasicInfo    UserBasicInfo      `json:"basicInfo"`
	}
	if ok, err := readState(page, "user.userPageData", &userPageData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal userPageData: %w", err)
	} else if !ok {
		return nil, fmt.Errorf("user.userPageData not found in __INITIAL_STATE__")
	}

	// 2. 获取用户帖子
	var notes [][]Feed
	if ok, err := readState(page, "user.notes", &notes); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("user.notes not found in __INITIAL_STATE__")
	}

	// 3. 当前 tab。读不到就按默认下标 0、不校验 tab 处理——原先的 JS 也是这么兜底的。
	var activeTab struct {
		Index int    `json:"index"`
		Query string `json:"query"`
	}
	if _, err := readState(page, "user.activeTab", &activeTab); err != nil {
		return nil, err
	}

	// tab 不符时报错，避免把别的 tab 的内容当成结果返回
	want := tab
	if want == "" {
		want = TabNotes
	}
	if activeTab.Query != "" && ProfileTab(activeTab.Query) != want {
		return nil, fmt.Errorf("当前 tab 为 %q，与请求的 %q 不符", activeTab.Query, want)
	}

	// 组装响应
	response := &UserProfileResponse{
		UserBasicInfo: userPageData.BasicInfo,
		Interactions:  userPageData.Interactions,
	}

	// 每个 tab 的内容存在各自的下标里，只取当前 tab 的，避免混入其他 tab
	if activeTab.Index >= 0 && activeTab.Index < len(notes) {
		response.Feeds = append(response.Feeds, notes[activeTab.Index]...)
	}

	// Notes listed on a profile carry pc_note, not pc_feed.
	noteSources.rememberFeeds(response.Feeds, xsecSourceNote, currentURL(page))

	return response, nil
}

func makeUserProfileURL(userID, xsecToken string, tab ProfileTab) string {
	return ActiveSite().UserProfileURL(userID, xsecToken, tab)
}

func (u *UserProfileAction) GetMyProfileViaSidebar(ctx context.Context, tab ProfileTab) (*UserProfileResponse, error) {
	page := u.page.Context(ctx).Timeout(60 * time.Second) // 重设被 .Context 清掉的 deadline

	// 创建导航动作
	navigate := NewNavigate(page)

	// 通过侧边栏导航到个人主页
	if err := navigate.ToProfilePage(ctx); err != nil {
		return nil, fmt.Errorf("failed to navigate to profile page via sidebar: %w", err)
	}

	// 等待页面加载完成并获取 __INITIAL_STATE__
	page.MustWaitStable()

	if err := u.selectTab(ctx, page, tab); err != nil {
		return nil, err
	}

	return u.extractUserProfileData(ctx, page, tab)
}

// selectTab 切到目标子 tab。「笔记」是默认 tab，无需点击。
func (u *UserProfileAction) selectTab(ctx context.Context, page *rod.Page, tab ProfileTab) error {
	if tab == "" || tab == TabNotes {
		return nil
	}

	label := tabLabel[tab]
	elems, err := page.Elements(`.reds-tab-item.sub-tab-list`)
	if err != nil {
		return fmt.Errorf("未找到主页子 tab: %w", err)
	}

	for _, elem := range elems {
		text, err := elem.Text()
		if err != nil || strings.TrimSpace(text) != label {
			continue
		}
		humanize.Delay(ctx, humanize.BeforeClick)
		if err := humanize.Click(elem); err != nil {
			return fmt.Errorf("切换到 %s 失败: %w", label, err)
		}
		humanize.Delay(ctx, humanize.AfterClick)
		page.MustWaitStable()
		return nil
	}
	return fmt.Errorf("未找到子 tab %q", label)
}
