package configs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestProfileDir 校验优先级：XHS_PROFILE_DIR > 会话文件同级目录下的 profile/。
func TestProfileDir(t *testing.T) {
	t.Run("环境变量优先", func(t *testing.T) {
		t.Setenv("XHS_PROFILE_DIR", "/custom/profile")
		assert.Equal(t, "/custom/profile", ProfileDir())
	})

	t.Run("空白的环境变量视为未设", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XHS_PROFILE_DIR", "   ")
		t.Setenv("COOKIES_PATH", filepath.Join(dir, "cookies.json"))

		assert.Equal(t, filepath.Join(dir, "profile"), ProfileDir())
	})

	t.Run("默认与会话文件同级", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XHS_PROFILE_DIR", "")
		t.Setenv("COOKIES_PATH", filepath.Join(dir, "data", "cookies.json"))

		assert.Equal(t, filepath.Join(dir, "data", "profile"), ProfileDir())
	})
}

func TestBrowserLifecycleFromEnv(t *testing.T) {
	clearEnv := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{"XHS_BROWSER_IDLE", "XHS_BROWSER_MAX_PAGES", "XHS_BROWSER_MAX_AGE"} {
			t.Setenv(k, "")
		}
	}

	t.Run("未设时用默认值", func(t *testing.T) {
		clearEnv(t)
		assert.Equal(t, DefaultBrowserLifecycle(), BrowserLifecycleFromEnv())
	})

	t.Run("覆盖全部三项", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XHS_BROWSER_IDLE", "90s")
		t.Setenv("XHS_BROWSER_MAX_PAGES", "5")
		t.Setenv("XHS_BROWSER_MAX_AGE", "2h")

		assert.Equal(t, BrowserLifecycle{
			IdleTimeout: 90 * time.Second,
			MaxPages:    5,
			MaxAge:      2 * time.Hour,
		}, BrowserLifecycleFromEnv())
	})

	t.Run("裸数字按秒解析", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XHS_BROWSER_IDLE", "45")
		assert.Equal(t, 45*time.Second, BrowserLifecycleFromEnv().IdleTimeout)
	})

	t.Run("显式零关闭对应策略", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XHS_BROWSER_IDLE", "0")
		t.Setenv("XHS_BROWSER_MAX_PAGES", "0")
		t.Setenv("XHS_BROWSER_MAX_AGE", "0")

		assert.Equal(t, BrowserLifecycle{}, BrowserLifecycleFromEnv())
	})

	t.Run("非法值忽略并回退默认", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XHS_BROWSER_IDLE", "soon")
		t.Setenv("XHS_BROWSER_MAX_PAGES", "-3")

		lc := BrowserLifecycleFromEnv()
		assert.Equal(t, DefaultBrowserLifecycle().IdleTimeout, lc.IdleTimeout)
		assert.Equal(t, DefaultBrowserLifecycle().MaxPages, lc.MaxPages)
	})
}
