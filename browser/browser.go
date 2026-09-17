package browser

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// language pinned on every launch: Accept-Language + navigator.languages.
// Kept as a constant so the probe harness and any future ICU/locale flag
// (issue #9) have a single source of truth.
const launchLanguage = "zh-CN"

// DefaultTimezone is the zone used when none is configured (issue #2).
//
// Deliberately not the host zone: the target site is Chinese, the egress IP is
// meant to be Chinese and navigator.languages says zh-CN, so a browser
// reporting Europe/London is a one-line mismatch check.
//
// It is the last fallback, not the usual path: the site preset decides the zone
// (mainland pins this one, rednote follows the host) and XHS_TIMEZONE overrides
// both. The Docker image no longer bakes a zone into itself for the same
// reason — see the timezone note in docker/README.md.
const DefaultTimezone = "Asia/Shanghai"

type browserConfig struct {
	// fingerprintSeed 固定指纹 seed；>0 时钉死，同账号每次同一套指纹。0 = 每次随机。
	fingerprintSeed int
	// proxy 代理地址；非空时启用。
	proxy string
	// timezone is the IANA zone reported by Intl and Date. Empty = DefaultTimezone.
	timezone string

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

	// launcherHook runs against the rod launcher just before it starts the
	// process. The manager uses it to capture the launcher, which is the only
	// handle that can kill a browser whose CDP connection has died.
	launcherHook func(*launcher.Launcher)

	// userDataDir is the persistent Chrome profile directory (issue #6).
	// Empty = rod picks a temp dir and deletes it on Close.
	//
	// Deliberately NOT a launchFlags entry. The fork only sets its
	// keepUserDataDir guard from headless_browser.WithUserDataDir, so a
	// "user-data-dir" key routed through ExtraFlags would reach the launcher
	// and then be deleted again by Cleanup() on Close — a profile that looks
	// persistent and silently is not.
	userDataDir string
}

type Option func(*browserConfig)

// WithProxy 设置代理（http/https/socks5）。空字符串视为不启用。
func WithProxy(proxy string) Option {
	return func(c *browserConfig) {
		c.proxy = proxy
	}
}

// WithTimezone pins the browser timezone (IANA name, e.g. "Asia/Shanghai").
// Empty falls back to DefaultTimezone — never to the host zone, which is the
// leak issue #2 is about. Env parsing lives in configs, as with proxy and seed.
func WithTimezone(tz string) Option {
	return func(c *browserConfig) {
		c.timezone = tz
	}
}

// WithUserDataDir pins the Chrome profile directory, so localStorage, IndexedDB
// and the live cookie jar survive a browser restart (issue #6). Empty keeps
// rod's throwaway temp profile.
//
// This must stay a real headless_browser option rather than a launch flag; see
// the browserConfig field comment.
func WithUserDataDir(dir string) Option {
	return func(c *browserConfig) {
		c.userDataDir = dir
	}
}

// WithLauncherHook registers a callback run against the rod launcher after
// every flag has been applied and before the process starts.
//
// Never call l.Preferences from it with a persistent profile: rod implements
// that by overwriting <profile>/Default/Preferences wholesale on every launch,
// which would throw away Chrome's own accumulated preferences.
func WithLauncherHook(hook func(*launcher.Launcher)) Option {
	return func(c *browserConfig) {
		c.launcherHook = hook
	}
}

// WithCookiesJSON seeds the browser with a cookie jar snapshot. The manager
// (issue #6) decides per launch whether to seed at all, so this cannot be read
// from disk unconditionally the way NewBrowser does it.
func WithCookiesJSON(raw string) Option {
	return func(c *browserConfig) {
		c.cookiesJSON = raw
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

		// Timezone (#2). The bundled binary honours --timezone and it moves
		// both Intl and Date together, which is what matters: spoofing that
		// changes Intl while Date keeps the host offset is itself detectable.
		// Do not combine with a per-page Emulation.setTimezoneOverride.
		"timezone": resolveTimezone(cfg),

		// ICU locale (#9). The CDP Accept-Language override reaches
		// navigator.languages and the request header but not ICU, so
		// Intl.DateTimeFormat().resolvedOptions().locale otherwise follows the
		// host and contradicts navigator.language. Both derive from
		// launchLanguage so there is one source of truth.
		// Measured caveat: on macOS neither flag moves ICU — Chrome takes its
		// default locale from the OS there — so the page hook also issues
		// Emulation.setLocaleOverride, which does. The flags stay because they
		// are what works on the Linux/Docker path.
		"lang":        launchLanguage,
		"accept-lang": launchLanguage,

		// Window geometry (#1): outerWidth/outerHeight only. screen.* and
		// devicePixelRatio come from the per-page CDP override below.
		"window-size": windowSizeFlag(deriveGeometry(cfg.fingerprintSeed, resolvePlatform())),
	}
}

// resolveTimezone returns the zone to launch with. Kept separate so the flag
// builder stays a straight map literal.
func resolveTimezone(cfg *browserConfig) string {
	if cfg.timezone != "" {
		return cfg.timezone
	}
	return DefaultTimezone
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
		// Per-page CDP setup: window metrics (#1) and ICU locale (#9), both of
		// which no launch flag on this build can deliver on its own.
		headless_browser.WithPageHook(pageSetupHook(cfg)),
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

	if cfg.launcherHook != nil {
		opts = append(opts, headless_browser.WithLauncherHook(cfg.launcherHook))
	}

	// Persistent profile (#6). An option, never a flag: only this option sets
	// the fork's keepUserDataDir guard that stops Close() deleting the dir.
	if cfg.userDataDir != "" {
		opts = append(opts, headless_browser.WithUserDataDir(cfg.userDataDir))
	}

	return opts
}

// pageSetupHook returns the per-page hook run on every page the browser opens.
//
// The geometry is computed once per config, so every page of a session reports
// the same monitor rather than re-rolling it per tab.
func pageSetupHook(cfg *browserConfig) func(*rod.Page) error {
	g := deriveGeometry(cfg.fingerprintSeed, resolvePlatform())
	metrics := newTextMetricsInstaller(cfg.fingerprintSeed)
	return func(page *rod.Page) error {
		// Headful is for a human: the QR login, and debugging. Pinning the
		// viewport there letterboxes the page inside a differently-sized OS
		// window, which looks like a broken non-responsive layout. The override
		// exists to make headless geometry coherent (#1); headless is also the
		// only mode Xiaohongshu ever sees, so skipping it here costs nothing.
		if cfg.headless {
			if err := applyGeometry(page, g); err != nil {
				return err
			}
		}
		// ICU locale (#9). navigator.language says zh-CN while
		// Intl.DateTimeFormat().resolvedOptions().locale otherwise reports the
		// host's — en-GB on the machine this was measured on — and the two
		// disagreeing is the tell. --lang does not move ICU on macOS; this
		// does, on both platforms.
		if err := (proto.EmulationSetLocaleOverride{Locale: launchLanguage}).Call(page); err != nil {
			return err
		}
		// Canvas text metrics (#15). The single deliberate exception to
		// WithStealthJS(false): the bundled build returns a signed near-zero
		// for every TextMetrics field, which no flag can fix and which is
		// visible to ordinary layout code. Calibrated here on about:blank and
		// injected before the first navigation. A failure is logged, not
		// fatal — an unrepaired browser is the status quo, not a broken one.
		if err := metrics.install(page); err != nil {
			logrus.Warnf("canvas text metrics shim not installed (#15): %v", err)
		}
		return nil
	}
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

// logLaunch prints the launch parameters worth seeing in a bug report.
func logLaunch(cfg *browserConfig) {
	if cfg.proxy != "" {
		logrus.Infof("Using proxy: %s", maskProxyCredentials(cfg.proxy))
	}
	if cfg.fingerprintSeed > 0 {
		logrus.Infof("fingerprint seed pinned: %d", cfg.fingerprintSeed)
	}
	if cfg.userDataDir != "" {
		logrus.Infof("browser profile: %s", cfg.userDataDir)
	}
	g := deriveGeometry(cfg.fingerprintSeed, resolvePlatform())
	logrus.Infof("browser timezone: %s; window %dx%d on a %dx%d screen @%gx",
		resolveTimezone(cfg), g.innerW, g.innerH, g.screenW, g.screenH, g.dpr)
}

// launch resolves the bundled binary and starts a browser from cfg, returning
// an error instead of panicking.
//
// headless_browser.New is built out of MustLaunch/MustConnect/MustSetCookies,
// so every failure mode — a held SingletonLock, a stale DevToolsActivePort, a
// malformed cookie jar — arrives as a panic. A long-lived server must not die
// because one launch failed, so they are recovered into errors here. This is
// the single launch path; NewBrowser is a panicking wrapper over it.
func launch(cfg *browserConfig) (b *headless_browser.Browser, err error) {
	// 只用内置浏览器，没有别的来源。二进制必须显式传给 go-rod，
	// 否则 rod 会自行下载一个默认 Chromium：它不是内置浏览器，也不认识下面
	// 这些 flag（未知 flag 被静默忽略，日志照样打印 "fingerprint enabled"），
	// 属于无声降级。宁可不启动，也不启动一个不对的浏览器。
	binPath, binErr := EnsureBrowser()
	if binErr != nil {
		return nil, fmt.Errorf("内置浏览器不可用，拒绝启动: %w", binErr)
	}
	cfg.binPath = binPath

	logLaunch(cfg)

	defer func() {
		if r := recover(); r != nil {
			b = nil
			err = fmt.Errorf("browser launch failed: %v", r)
		}
	}()

	return headless_browser.New(buildOptions(cfg)...), nil
}

// NewBrowser gathers the cookie jar from disk and launches with the option set
// from buildOptions. It is deliberately thin: everything worth testing lives in
// launchFlags/buildOptions.
//
// It panics on failure, which is what its callers (one-shot CLIs and tests)
// want. The long-lived server goes through Manager, which uses launch directly.
func NewBrowser(headless bool, options ...Option) *headless_browser.Browser {
	cfg := newConfig(headless, options...)
	if cfg.cookiesJSON == "" {
		cfg.cookiesJSON = loadCookiesJSON()
	}

	b, err := launch(cfg)
	if err != nil {
		panic(err.Error())
	}
	return b
}
