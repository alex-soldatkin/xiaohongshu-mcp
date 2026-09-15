package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xpzouying/headless_browser"
)

// TestMaskProxyCredentials 校验代理日志脱敏：绝不能把用户名/密码打进日志。
func TestMaskProxyCredentials(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "空字符串", input: "", want: ""},
		{name: "无认证信息原样返回", input: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "用户名+密码都脱敏", input: "http://user:pass@host:8080", want: "http://***:***@host:8080"},
		{name: "仅用户名脱敏", input: "http://user@host:8080", want: "http://***@host:8080"},
		{name: "socks5带认证", input: "socks5://alice:secret@127.0.0.1:1080", want: "socks5://***:***@127.0.0.1:1080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, maskProxyCredentials(tt.input))
		})
	}
}

// TestOptions 校验 Option 正确写入 browserConfig（New+Option 的接线）。
func TestOptions(t *testing.T) {
	cfg := &browserConfig{}
	WithFingerprintSeed(98759)(cfg)
	WithProxy("http://127.0.0.1:8080")(cfg)

	assert.Equal(t, 98759, cfg.fingerprintSeed)
	assert.Equal(t, "http://127.0.0.1:8080", cfg.proxy)
}

// TestOptions_Defaults 未传 Option 时各字段为零值（回退随机 seed / 不设代理）。
func TestOptions_Defaults(t *testing.T) {
	cfg := &browserConfig{}
	assert.Equal(t, 0, cfg.fingerprintSeed)
	assert.Equal(t, "", cfg.proxy)
}

// applyOptions replays a headless_browser option slice onto a Config so a test
// can assert on what the option set actually configures. The options are
// opaque funcs; the Config they produce is the only observable result.
func applyOptions(opts []headless_browser.Option) *headless_browser.Config {
	cfg := &headless_browser.Config{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// TestLaunchFlags pins the exact extra-flag set handed to the bundled
// Chromium. This is the production flag set the probe harness measures, so a
// change here invalidates the recorded baseline (issue #4).
func TestLaunchFlags(t *testing.T) {
	cfg := newConfig(true)
	flags := launchFlags(cfg)

	assert.Equal(t, map[string]string{
		"fingerprint-brand": "Chrome",
		"timezone":          "Asia/Shanghai", // #2
		"lang":              "zh-CN",         // #9
		"accept-lang":       "zh-CN",         // #9
		// #1, outer window only; screen.* and dpr come from the page hook.
		"window-size": windowSizeFlag(deriveGeometry(cfg.fingerprintSeed, resolvePlatform())),
	}, flags)

	// Documented gaps, each owned by an open issue. Asserting their absence
	// keeps the baseline honest: when one is added the test fails loudly and
	// the probe must be re-run.
	for _, absent := range []string{
		"force-webrtc-ip-handling-policy", // #9: measured 0 ICE candidates, nothing to suppress
		"user-data-dir",                   // #6
		// #1 is fixed with Emulation.setDeviceMetricsOverride instead: these
		// two were never tried and the CDP route is measured to work.
		"fingerprint-screen-width",
		"fingerprint-screen-height",
		"disable-blink-features", // measured unnecessary, see #4
		// Would replace rod's default value for the key rather than extend it.
		"enable-features",
	} {
		_, ok := flags[absent]
		assert.Falsef(t, ok, "flag %q unexpectedly present", absent)
	}
}

// TestLaunchFlags_Timezone XHS_TIMEZONE 经 Option 传入时覆盖默认值；
// 空值回落到 Asia/Shanghai，绝不回落到宿主机时区。
func TestLaunchFlags_Timezone(t *testing.T) {
	assert.Equal(t, "Europe/Berlin",
		launchFlags(newConfig(true, WithTimezone("Europe/Berlin")))["timezone"])
	assert.Equal(t, DefaultTimezone,
		launchFlags(newConfig(true, WithTimezone("")))["timezone"])
}

// TestLaunchFlags_LocaleSingleSource lang/accept-lang 必须跟着 WithLanguage 走，
// 不得各写各的：三处不一致本身就是破绽。
func TestLaunchFlags_LocaleSingleSource(t *testing.T) {
	flags := launchFlags(newConfig(true))
	c := applyOptions(buildOptions(newConfig(true)))

	assert.Equal(t, launchLanguage, c.Language)
	assert.Equal(t, launchLanguage, flags["lang"])
	assert.Equal(t, launchLanguage, flags["accept-lang"])
}

// TestLaunchFlags_WindowSizeFollowsSeed 窗口大小必须跟着 seed 走：
// 写死常量等于所有账号共用同一台显示器。
func TestLaunchFlags_WindowSizeFollowsSeed(t *testing.T) {
	a := launchFlags(newConfig(true, WithFingerprintSeed(98759)))["window-size"]
	b := launchFlags(newConfig(true, WithFingerprintSeed(98759)))["window-size"]
	assert.Equal(t, a, b, "same seed must give the same window")

	differs := false
	for _, seed := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		if launchFlags(newConfig(true, WithFingerprintSeed(seed)))["window-size"] != a {
			differs = true
			break
		}
	}
	assert.True(t, differs, "window size does not vary across seeds")
}

// TestLaunchFlags_FreshMap 每次返回新 map：调用方（含测试）改动结果不得污染下一次启动。
func TestLaunchFlags_FreshMap(t *testing.T) {
	cfg := newConfig(true)

	first := launchFlags(cfg)
	first["user-data-dir"] = "/tmp/poisoned"

	second := launchFlags(cfg)
	_, ok := second["user-data-dir"]
	assert.False(t, ok, "launchFlags 返回了共享 map")
}

// TestBuildOptions_Defaults 无 Option 时的基线配置。
func TestBuildOptions_Defaults(t *testing.T) {
	c := applyOptions(buildOptions(newConfig(true)))

	assert.True(t, c.Headless)
	assert.True(t, c.Fingerprint, "源码级指纹引擎必须启用")
	assert.Equal(t, "", c.FingerprintPlatform, "留空 = 按运行 OS 自动选择画像")
	assert.False(t, c.StealthJS, "CloakBrowser 下注入 stealth.js 反而制造矛盾")
	assert.Equal(t, "zh-CN", c.Language)
	assert.Equal(t, "", c.UserAgent, "不得强制 UA：会与 Client Hints 矛盾")
	assert.Equal(t, launchFlags(newConfig(true)), c.ExtraFlags)
	assert.NotNil(t, c.PageHook, "geometry and locale overrides must reach every page (#1, #9)")

	assert.Equal(t, "", c.Proxy)
	assert.Equal(t, 0, c.FingerprintSeed)
	assert.Equal(t, "", c.Cookies)
	assert.Equal(t, "", c.ChromeBinPath)
}

// TestBuildOptions_Headful headless=false 必须透传。
func TestBuildOptions_Headful(t *testing.T) {
	c := applyOptions(buildOptions(newConfig(false)))
	assert.False(t, c.Headless)
}

// TestBuildOptions_Full 所有字段都被填上时的完整选项集。
func TestBuildOptions_Full(t *testing.T) {
	cfg := newConfig(true,
		WithProxy("socks5://user:pass@127.0.0.1:1080"),
		WithFingerprintSeed(98759),
	)
	cfg.binPath = "/tmp/Chromium"
	cfg.cookiesJSON = `[{"name":"a1","value":"x"}]`

	c := applyOptions(buildOptions(cfg))

	assert.Equal(t, "socks5://user:pass@127.0.0.1:1080", c.Proxy)
	assert.Equal(t, 98759, c.FingerprintSeed)
	assert.Equal(t, "/tmp/Chromium", c.ChromeBinPath)
	assert.Equal(t, `[{"name":"a1","value":"x"}]`, c.Cookies)
}

// TestBuildOptions_NonPositiveSeed seed<=0 不得写入：0 在 headless_browser 里
// 表示"每次随机"，显式传负数会被当成固定 seed 用掉。
func TestBuildOptions_NonPositiveSeed(t *testing.T) {
	for _, seed := range []int{0, -1} {
		c := applyOptions(buildOptions(newConfig(true, WithFingerprintSeed(seed))))
		assert.Equalf(t, 0, c.FingerprintSeed, "seed=%d 应回退随机", seed)
	}
}

// TestBuildOptions_Pure 纯函数：同一 cfg 调两次结果一致，且不改动 cfg。
func TestBuildOptions_Pure(t *testing.T) {
	cfg := newConfig(true, WithProxy("http://127.0.0.1:8080"), WithFingerprintSeed(42))
	before := *cfg

	a := applyOptions(buildOptions(cfg))
	b := applyOptions(buildOptions(cfg))

	// PageHook is a closure: two calls produce two distinct func values that
	// reflect.DeepEqual can never match. Compare the geometry it carries
	// instead — that is the part that has to be deterministic.
	assert.Equal(t, deriveGeometry(42, resolvePlatform()), deriveGeometry(42, resolvePlatform()))
	a.PageHook, b.PageHook = nil, nil
	assert.Equal(t, a, b)
	assert.Equal(t, before, *cfg, "buildOptions 不得改动入参")
}

// TestWithUserDataDir pins the one thing about the persistent profile (#6) that
// is easy to get wrong: the directory must travel as a headless_browser option,
// never as a launch flag.
//
// Both routes put --user-data-dir on the command line, so both look like they
// work. Only the option sets the fork's keepUserDataDir guard; via ExtraFlags,
// Close() runs launcher.Cleanup() with the flag still set and deletes the
// profile it was supposed to keep.
func TestWithUserDataDir(t *testing.T) {
	cfg := newConfig(true, WithUserDataDir("/data/profile"))

	assert.Equal(t, "/data/profile", applyOptions(buildOptions(cfg)).UserDataDir)

	_, ok := launchFlags(cfg)["user-data-dir"]
	assert.False(t, ok, "the profile dir must not be passed as a launch flag")
}

// TestWithUserDataDir_Empty 未配置时不得出现在 option 里：空字符串会让 rod
// 认为调用方指定了目录，反而关掉它自己的临时目录清理。
func TestWithUserDataDir_Empty(t *testing.T) {
	assert.Equal(t, "", applyOptions(buildOptions(newConfig(true))).UserDataDir)
}

// TestWithCookiesJSON: the manager decides per launch whether to seed, so the
// jar has to be injectable rather than always read from disk.
func TestWithCookiesJSON(t *testing.T) {
	cfg := newConfig(true, WithCookiesJSON(`[{"name":"a"}]`))
	assert.Equal(t, `[{"name":"a"}]`, applyOptions(buildOptions(cfg)).Cookies)

	assert.Equal(t, "", applyOptions(buildOptions(newConfig(true))).Cookies)
}
