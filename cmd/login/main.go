// Command login opens a visible browser, walks the QR login and stores the
// session.
//
// It shares the persistent profile with the server (issue #6), so the login it
// performs is the one the server will find. That also means it must not run
// while the server is up: Chrome allows exactly one process per profile, and
// the second one is refused with an error naming the pid holding the lock.
package main

import (
	"context"
	"flag"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/xiaohongshu"
)

func main() {
	// -site names the deployment to log in to: "xiaohongshu" or "rednote".
	// A fresh install has no session file and no cookies to learn from, so a
	// first rednote login has no other way to say which site it means.
	// Precedence afterwards is the flag, then XHS_SITE, then the session file.
	var siteName string
	flag.StringVar(&siteName, "site", "", "站点：xiaohongshu 或 rednote，留空则读取 XHS_SITE 或会话文件")
	flag.Parse()

	// 登录的时候，需要界面，所以不能无头模式。
	// 登录与后续运行共用同一个 seed：首次登录生成并写入会话文件，之后一直复用。
	store := cookies.NewLoadCookie(cookies.GetCookiesFilePath())

	site, reason, err := xiaohongshu.ResolveSite(siteName, store)
	if err != nil {
		logrus.Fatalf("%v", err)
	}
	xiaohongshu.SetSite(site)
	logrus.Infof("site: %s (%s), %s", site.Name, site.Domain, reason)

	profileDir := configs.ProfileDir()
	logrus.Infof("browser profile directory: %s", profileDir)

	// Same manager as the server, so seeding, cookie export and the seed marker
	// are one code path rather than two that drift apart.
	manager := browser.NewManager(browser.ManagerConfig{
		Headless: false,
		Options: []browser.Option{
			browser.WithFingerprintSeed(configs.ResolveFingerprintSeed(store)),
			browser.WithProxy(configs.ProxyFromEnv()),
			browser.WithTimezone(loginTimezone(site)),
		},
		ProfileDir: profileDir,
		Session:    store,
		Site:       site.Name,
		SiteDomain: site.Domain,
	})
	ctx := context.Background()
	defer manager.Shutdown(ctx)

	lease, err := manager.Lease(ctx)
	if err != nil {
		logrus.Fatalf("failed to start the browser: %v", err)
	}
	defer lease.Release()

	action := xiaohongshu.NewLogin(lease.Page)

	status, err := action.CheckLoginStatus(ctx)
	if err != nil {
		logrus.Fatalf("failed to check login status: %v", err)
	}

	logrus.Infof("当前登录状态: %v", status)

	if status {
		return
	}

	// 开始登录流程
	logrus.Info("开始登录流程...")
	if err = action.Login(ctx); err != nil {
		logrus.Fatalf("登录失败: %v", err)
	}
	if err := manager.ExportCookies(); err != nil {
		logrus.Fatalf("failed to save cookies: %v", err)
	}

	// 再次检查登录状态确认成功
	status, err = action.CheckLoginStatus(ctx)
	if err != nil {
		logrus.Fatalf("failed to check login status after login: %v", err)
	}

	if status {
		logrus.Info("登录成功！")
	} else {
		logrus.Error("登录流程完成但仍未登录")
	}
}

// loginTimezone mirrors the server's choice: XHS_TIMEZONE wins, otherwise the
// site's default zone. The login browser must present the same identity the
// server will, or the first request after login looks like a different device.
func loginTimezone(site xiaohongshu.Site) string {
	if tz := configs.TimezoneFromEnv(); tz != "" {
		return tz
	}
	return site.BrowserTimezone()
}
