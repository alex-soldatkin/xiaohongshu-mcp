package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

type FeedsListAction struct {
	page *rod.Page
}

func NewFeedsListAction(page *rod.Page) *FeedsListAction {
	pp := page.Timeout(60 * time.Second)

	// Session entry point, not a deep link: no referrer to claim.
	pp.MustNavigate(urlHome)
	pp.MustWaitDOMStable()

	return &FeedsListAction{page: pp}
}

// GetFeedsList 获取页面的 Feed 列表数据
func (f *FeedsListAction) GetFeedsList(ctx context.Context) ([]Feed, error) {
	// 重设超时：.Context(ctx) 会替换掉构造函数里 Timeout(60s) 的 deadline
	page := f.page.Context(ctx).Timeout(60 * time.Second)

	// 轮询等 __INITIAL_STATE__.feed 注水就绪（替代固定 1s，治偶发 ErrNoFeeds）
	var (
		result  string
		lastErr error
	)
	deadline := time.Now().Add(8 * time.Second)
	for {
		result, lastErr = readStateJSON(page, "feed.feeds")
		if result != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if result == "" {
		// A failed eval and "the page genuinely has none" are different things;
		// do not report the former as ErrNoFeeds.
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.ErrNoFeeds
	}

	var feeds []Feed
	if err := json.Unmarshal([]byte(result), &feeds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal feeds: %w", err)
	}

	notes := onlyNotes(feeds)
	noteSources.rememberFeeds(notes, xsecSourceFeed, urlExplore)

	return notes, nil
}
