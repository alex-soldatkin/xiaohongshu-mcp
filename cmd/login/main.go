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
	flag.Parse()

	// 登录的时候，需要界面，所以不能无头模式。
	// 登录与后续运行共用同一个 seed：首次登录生成并写入会话文件，之后一直复用。
	store := cookies.NewLoadCookie(cookies.GetCookiesFilePath())

	profileDir := configs.ProfileDir()
	logrus.Infof("browser profile directory: %s", profileDir)

	// Same manager as the server, so seeding, cookie export and the seed marker
	// are one code path rather than two that drift apart.
	manager := browser.NewManager(browser.ManagerConfig{
		Headless: false,
		Options: []browser.Option{
			browser.WithFingerprintSeed(configs.ResolveFingerprintSeed(store)),
			browser.WithProxy(configs.ProxyFromEnv()),
			browser.WithTimezone(configs.TimezoneFromEnv()),
		},
		ProfileDir: profileDir,
		Session:    store,
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
