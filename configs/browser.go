package configs

import (
	"os"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

var (
	useHeadless = true

	fingerprintSeed = 0

	proxy = ""

	// timezone 浏览器时区；空 = 用浏览器层的默认值（Asia/Shanghai），而不是宿主机时区。
	timezone = ""
)

func InitHeadless(h bool) {
	useHeadless = h
}

// IsHeadless 是否无头模式。
func IsHeadless() bool {
	return useHeadless
}

func SetFingerprintSeed(s int) {
	fingerprintSeed = s
}

func FingerprintSeed() int {
	return fingerprintSeed
}

// FingerprintSeedFromEnv 从 XHS_FP_SEED 环境变量解析固定 seed。
// 未设或非法返回 0（回退随机）。env 读取集中在配置层，浏览器工厂只收 Option。
func FingerprintSeedFromEnv() int {
	s := os.Getenv("XHS_FP_SEED")
	if s == "" {
		return 0
	}
	seed, err := strconv.Atoi(s)
	if err != nil || seed <= 0 {
		logrus.Warnf("invalid XHS_FP_SEED=%q, ignored (fallback to random seed)", s)
		return 0
	}
	return seed
}

func SetProxy(p string) {
	proxy = p
}

func Proxy() string {
	return proxy
}

// ProxyFromEnv 从 XHS_PROXY 环境变量读取代理地址。env 读取集中在配置层。
func ProxyFromEnv() string {
	return os.Getenv("XHS_PROXY")
}

func SetTimezone(tz string) {
	timezone = tz
}

// Timezone 浏览器时区（IANA 名）。空表示未配置，由浏览器层套用默认值。
func Timezone() string {
	return timezone
}

// TimezoneFromEnv 从 XHS_TIMEZONE 读取时区（如 "Asia/Shanghai"）。
// Empty or malformed returns "", which the browser layer turns into its own
// default — never the host zone, which is the leak in issue #2.
//
// Validation is syntactic on purpose: time.LoadLocation would make this depend
// on a tzdata database being present, and a missing database would then reject
// a perfectly good zone name that Chromium's own ICU copy understands.
func TimezoneFromEnv() string {
	tz := strings.TrimSpace(os.Getenv("XHS_TIMEZONE"))
	if tz == "" {
		return ""
	}
	if !validTimezoneName(tz) {
		logrus.Warnf("invalid XHS_TIMEZONE=%q, ignored (fallback to default zone)", tz)
		return ""
	}
	return tz
}

// validTimezoneName 只做形状校验：IANA 名形如 Asia/Shanghai、UTC、GMT+8。
func validTimezoneName(tz string) bool {
	if len(tz) > 64 || strings.HasPrefix(tz, "/") || strings.HasSuffix(tz, "/") {
		return false
	}
	for _, r := range tz {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '_', r == '-', r == '+':
		default:
			return false
		}
	}
	return true
}
