package browser

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// language pinned on every launch: Accept-Language + navigator.languages.
// Kept as a constant so the probe harness and any future ICU/locale flag
// (issue #9) have a single source of truth.
const launchLanguage = "zh-CN"

type browserConfig struct {
	// fingerprintSeed 固定指纹 seed；>0 时钉死，同账号每次同一套指纹。0 = 每次随机。
	fingerprintSeed int
	// proxy 代理地址；非空时启用。
	proxy string

	// headless runs the browser without a window. Part of the config (rather
	// than a NewBrowser argument threaded separately) so that buildOptions is
	// a pure function of one value.
	headless bool
	// binPath is the bundled Chromium executable. Resolved by the caller via
	// EnsureBrowser so that buildOptions performs no I/O.
	binPath string
	// cookiesJSON is the raw cookie jar to seed. Empty = seed nothing.
	// Read from disk by the caller, again to keep buildOptions pure.
	cookiesJSON string
}

type Option func(*browserConfig)

// WithProxy 设置代理（http/https/socks5）。空字符串视为不启用。
func WithProxy(proxy string) Option {
	return func(c *browserConfig) {
		c.proxy = proxy
	}
}

// WithFingerprintSeed 设置 seed，seed<=0 视为未设，回退每次随机。
func WithFingerprintSeed(seed int) Option {
	return func(c *browserConfig) {
		c.fingerprintSeed = seed
	}
}

// maskProxyCredentials masks username and password in proxy URL for safe logging.
func maskProxyCredentials(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return proxyURL
	}
	cred := "***"
	if _, hasPassword := u.User.Password(); hasPassword {
		cred = "***:***"
	}
	// 直接在原串替换 userinfo，避免 url.String() 把 * 编码成 %2A（日志变乱码）。
	return strings.Replace(proxyURL, u.User.String()+"@", cred+"@", 1)
}

// newConfig applies the options onto a config with the given headless mode.
// It does no I/O: binPath and cookiesJSON stay empty and are filled by NewBrowser.
func newConfig(headless bool, options ...Option) *browserConfig {
	cfg := &browserConfig{headless: headless}
	for _, opt := range options {
		opt(cfg)
	}
	return cfg
}

// launchFlags returns the extra command-line flags passed straight through to
// the bundled Chromium. Keys carry no leading "--".
//
// Pure: no I/O, no globals, a fresh map every call. The probe harness in
// probe_integration_test.go launches with exactly this map, so a baseline
// measurement always describes the real production flag set rather than an
// approximation of it.
//
// Note: WithExtraFlags REPLACES rod's default value for a key it collides
// with. If anything ever adds "enable-features" here it must re-include
// "NetworkService,NetworkServiceInProcess".
func launchFlags(cfg *browserConfig) map[string]string {
	// 品牌报 Chrome。
	// 注：hardware-concurrency 不设，交给 seed 派生。
	return map[string]string{
		"fingerprint-brand": "Chrome",
	}
}

// buildOptions turns a browserConfig into the headless_browser option set.
//
// Pure: the returned options are a function of cfg alone. Side effects
// (resolving the browser binary, reading cookies.json, logging) belong to
// NewBrowser. Splitting them apart is what lets unit tests assert on the exact
// option set without launching a browser.
func buildOptions(cfg *browserConfig) []headless_browser.Option {
	opts := []headless_browser.Option{
		headless_browser.WithHeadless(cfg.headless),
		// 用内置浏览器的默认配置，不强制 UA。
		headless_browser.WithFingerprint(""), // 空 = 按运行 OS 自动：Linux→windows，mac→macos
		headless_browser.WithStealthJS(false),
		headless_browser.WithLanguage(launchLanguage), // 面向小红书
		headless_browser.WithExtraFlags(launchFlags(cfg)),
	}

	if cfg.binPath != "" {
		opts = append(opts, headless_browser.WithChromeBinPath(cfg.binPath))
	}

	// 代理（由调用方经 Option 传入，env 读取放在入口层）。
	if cfg.proxy != "" {
		opts = append(opts, headless_browser.WithProxy(cfg.proxy))
	}

	// 固定指纹 seed（由调用方经 Option 传入，env 读取放在入口层）。
	if cfg.fingerprintSeed > 0 {
		opts = append(opts, headless_browser.WithFingerprintSeed(cfg.fingerprintSeed))
	}

	if cfg.cookiesJSON != "" {
		opts = append(opts, headless_browser.WithCookies(cfg.cookiesJSON))
	}

	return opts
}

// loadCookiesJSON reads the cookie jar snapshot from disk. Returns "" when it
// is missing or unreadable — a fresh install has no cookies yet, which is not
// an error.
func loadCookiesJSON() string {
	cookiePath := cookies.GetCookiesFilePath()
	data, err := cookies.NewLoadCookie(cookiePath).LoadCookies()
	if err != nil {
		logrus.Warnf("failed to load cookies: %v", err)
		return ""
	}
	logrus.Debugf("loaded cookies from file successfully")
	return string(data)
}

// NewBrowser resolves the bundled binary, gathers the cookie jar and launches
// with the option set from buildOptions. It is deliberately thin: everything
// worth testing lives in launchFlags/buildOptions.
func NewBrowser(headless bool, options ...Option) *headless_browser.Browser {
	cfg := newConfig(headless, options...)

	// 只用内置浏览器，没有别的来源。二进制必须显式传给 go-rod，
	// 否则 rod 会自行下载一个默认 Chromium：它不是内置浏览器，也不认识下面
	// 这些 flag（未知 flag 被静默忽略，日志照样打印 "fingerprint enabled"），
	// 属于无声降级。宁可不启动，也不启动一个不对的浏览器。
	binPath, err := EnsureBrowser()
	if err != nil {
		panic(fmt.Sprintf("内置浏览器不可用，拒绝启动: %v", err))
	}
	cfg.binPath = binPath
	cfg.cookiesJSON = loadCookiesJSON()

	if cfg.proxy != "" {
		logrus.Infof("Using proxy: %s", maskProxyCredentials(cfg.proxy))
	}
	if cfg.fingerprintSeed > 0 {
		logrus.Infof("fingerprint seed pinned: %d", cfg.fingerprintSeed)
	}

	return headless_browser.New(buildOptions(cfg)...)
}
