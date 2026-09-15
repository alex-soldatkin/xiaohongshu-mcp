package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDeriveGeometry_Sane 每个 seed 派生出的窗口都必须物理上可能：
// inner <= screen，avail < screen（系统栏），inner < avail（浏览器自身 UI）。
// 这正是 issue #1 里那三条互相矛盾的观测的反面。
func TestDeriveGeometry_Sane(t *testing.T) {
	for _, platform := range []string{"macos", "windows"} {
		for seed := 0; seed < 500; seed++ {
			g := deriveGeometry(seed, platform)

			assert.LessOrEqualf(t, g.innerW, g.screenW, "%s seed=%d innerW>screenW", platform, seed)
			assert.LessOrEqualf(t, g.innerH, g.screenH, "%s seed=%d innerH>screenH", platform, seed)
			assert.Lessf(t, g.availH, g.screenH, "%s seed=%d availH must leave room for the system bar", platform, seed)
			assert.Lessf(t, g.innerH, g.availH, "%s seed=%d innerH must leave room for browser chrome", platform, seed)
			assert.Greaterf(t, g.innerH, 400, "%s seed=%d viewport collapsed", platform, seed)
		}
	}
}

// TestDeriveGeometry_DPRMatchesPlatform DPR 必须与 fingerprint-platform 自洽：
// MacIntel 画像报 devicePixelRatio=1 是 Retina 时代不存在的机器（issue #1）。
func TestDeriveGeometry_DPRMatchesPlatform(t *testing.T) {
	for seed := 0; seed < 500; seed++ {
		assert.Equalf(t, float64(2), deriveGeometry(seed, "macos").dpr, "macos seed=%d", seed)

		dpr := deriveGeometry(seed, "windows").dpr
		assert.Truef(t, dpr == 1 || dpr == 1.25, "windows seed=%d dpr=%g", seed, dpr)
	}
}

// TestDeriveGeometry_StablePerSeed 同一账号每次启动同一块屏幕。
func TestDeriveGeometry_StablePerSeed(t *testing.T) {
	for _, seed := range []int{1, 98759, 1 << 30} {
		assert.Equal(t, deriveGeometry(seed, "macos"), deriveGeometry(seed, "macos"))
	}
}

// TestDeriveGeometry_VariesBySeed 不同账号不得共用同一套几何：
// 固定常量会让所有账号看起来是同一台机器。
func TestDeriveGeometry_VariesBySeed(t *testing.T) {
	seen := map[geometry]bool{}
	for seed := 1; seed <= 2000; seed++ {
		seen[deriveGeometry(seed, "macos")] = true
	}
	// 5 张分辨率表 x 45 档 chrome 高度 x 50 档 Dock 预留，不同值应远多于 1 个。
	assert.Greater(t, len(seen), 50, "geometry barely varies across seeds")
}

// TestPickScreen_RespectsWeights 加权表要真的按权重分布，而不是永远命中第一项。
func TestPickScreen_RespectsWeights(t *testing.T) {
	counts := map[int]int{}
	for h := 0; h < 10000; h++ {
		counts[pickScreen(macScreens, uint64(h)).w]++
	}
	assert.Len(t, counts, len(macScreens), "some table entries are unreachable")
	assert.Greater(t, counts[1512], counts[1280], "weights not respected")
}

// TestSeedHash_SaltSeparatesFields 同一 seed 下不同字段必须独立取值，
// 否则屏幕尺寸和 chrome 高度会在所有账号之间同步变化。
func TestSeedHash_SaltSeparatesFields(t *testing.T) {
	assert.NotEqual(t, seedHash(98759, "screen"), seedHash(98759, "chrome"))
	assert.Equal(t, seedHash(98759, "screen"), seedHash(98759, "screen"))
}
