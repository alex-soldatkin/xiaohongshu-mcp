package main

import (
	"context"
	"flag"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/pkg/store"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"

	// Linked for its side effect: it registers the postgres:// and
	// postgresql:// schemes with store.Open. Nothing here calls into it.
	_ "github.com/xpzouying/xiaohongshu-mcp/pkg/store/pgstore"
)

// version 构建版本号，发布时通过 -ldflags "-X main.version=vX.Y.Z" 注入。
var version = "dev"

func main() {
	var (
		headless bool
		port     string
		token    string
	)
	flag.BoolVar(&headless, "headless", true, "是否无头模式")
	flag.StringVar(&port, "port", ":18060", "端口")
	flag.StringVar(&token, "token", "", "鉴权 Token，留空则读取 AUTH_TOKEN")
	flag.Parse()
	if token == "" {
		token = os.Getenv("AUTH_TOKEN")
	}

	logrus.Infof("xiaohongshu-mcp version: %s", version)

	// 只用内置浏览器。启动时就备好，缺它直接退出，不拖到第一个请求才失败。
	binPath, err := browser.EnsureBrowser()
	if err != nil {
		logrus.Fatalf("%v", err)
	}
	logrus.Infof("using browser binary: %s", binPath)

	configs.InitHeadless(headless)

	session := cookies.NewLoadCookie(cookies.GetCookiesFilePath())

	// Which deployment this process talks to, resolved exactly once: the
	// session file can be deleted at runtime by reset login, and re-resolving
	// afterwards would silently switch sites mid-run. A disagreement between
	// an explicit XHS_SITE and the saved cookies is fatal on purpose — the
	// tool cannot work in that state, and carrying on would trip the risk
	// detector into a cooldown on the first navigation (issue #18).
	site, reason, err := xiaohongshu.ResolveSite("", session)
	if err != nil {
		logrus.Fatalf("%v", err)
	}
	xiaohongshu.SetSite(site)
	logrus.Infof("site: %s (%s), %s", site.Name, site.Domain, reason)

	// 入口层解析出 seed 和代理，经 configs 透传给浏览器工厂。
	// seed 取值：环境变量 > 会话文件 > 新生成并写回，保证同一账号每次启动一致。
	configs.SetFingerprintSeed(configs.ResolveFingerprintSeed(session))
	configs.SetProxy(configs.ProxyFromEnv())
	// 时区独立于宿主机（issue #2）。XHS_TIMEZONE 优先；未设时用站点的默认值：
	// 国内站仍是 Asia/Shanghai，海外站跟随运维所在时区——海外账号、海外出口
	// IP 却自报上海，是同一种不自洽，只是符号反了（issue #18）。
	configs.SetTimezone(resolveTimezone(site))

	// Persistence layer (issue #7). XHS_DATABASE_URL unset is the default
	// deployment: store.Open returns a no-op store and behaviour is unchanged.
	// Set but unreachable is fatal, mirroring the missing-browser check above
	// — degrading quietly would leave an operator believing the cache works.
	databaseURL := os.Getenv("XHS_DATABASE_URL")
	dataStore, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		logrus.Fatalf("persistence: %v", err)
	}
	defer func() { _ = dataStore.Close() }()
	if databaseURL != "" {
		logrus.Infof("persistence: using %s", store.Redact(databaseURL))
	}

	// 初始化服务
	xiaohongshuService := NewXiaohongshuService(WithStore(dataStore))

	// 创建并启动应用服务器
	appServer := NewAppServer(xiaohongshuService, token)
	if err := appServer.Start(port); err != nil {
		logrus.Fatalf("failed to run server: %v", err)
	}
}

// resolveTimezone picks the browser timezone: XHS_TIMEZONE if the operator set
// one, otherwise the active site's default. An empty result means "no
// opinion", which the browser layer turns into its own default rather than
// into the host zone.
func resolveTimezone(site xiaohongshu.Site) string {
	if tz := configs.TimezoneFromEnv(); tz != "" {
		return tz
	}
	tz := site.BrowserTimezone()
	if tz == "" {
		logrus.Warnf("timezone: %s wants the host zone but the host cannot name it; falling back to the browser default", site.Name)
		return ""
	}
	logrus.Infof("timezone: %s (default for %s)", tz, site.Name)
	return tz
}
