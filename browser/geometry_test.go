package browser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDeriveGeometry_Sane every window derived from a seed must be physically
// possible: inner <= screen, avail < screen (system bar), inner < avail (the
// browser's own UI). This is the inverse of the three mutually contradictory
// observations recorded in issue #1.
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

// TestDeriveGeometry_DPRMatchesPlatform the DPR must be consistent with the
// fingerprint platform: a MacIntel profile reporting devicePixelRatio=1 is a
// machine that does not exist in the Retina era (issue #1).
func TestDeriveGeometry_DPRMatchesPlatform(t *testing.T) {
	for seed := 0; seed < 500; seed++ {
		assert.Equalf(t, float64(2), deriveGeometry(seed, "macos").dpr, "macos seed=%d", seed)

		dpr := deriveGeometry(seed, "windows").dpr
		assert.Truef(t, dpr == 1 || dpr == 1.25, "windows seed=%d dpr=%g", seed, dpr)
	}
}

// TestDeriveGeometry_StablePerSeed the same account gets the same screen on
// every launch.
func TestDeriveGeometry_StablePerSeed(t *testing.T) {
	for _, seed := range []int{1, 98759, 1 << 30} {
		assert.Equal(t, deriveGeometry(seed, "macos"), deriveGeometry(seed, "macos"))
	}
}

// TestDeriveGeometry_VariesBySeed different accounts must not share one
// geometry: fixed constants would make every account look like the same
// machine.
func TestDeriveGeometry_VariesBySeed(t *testing.T) {
	seen := map[geometry]bool{}
	for seed := 1; seed <= 2000; seed++ {
		seen[deriveGeometry(seed, "macos")] = true
	}
	// 5 resolution tables x 45 chrome heights x 50 Dock reservations, so the
	// number of distinct values should be far greater than one.
	assert.Greater(t, len(seen), 50, "geometry barely varies across seeds")
}

// TestPickScreen_RespectsWeights the weighted table must really distribute by
// weight instead of always hitting the first entry.
func TestPickScreen_RespectsWeights(t *testing.T) {
	counts := map[int]int{}
	for h := 0; h < 10000; h++ {
		counts[pickScreen(macScreens, uint64(h)).w]++
	}
	assert.Len(t, counts, len(macScreens), "some table entries are unreachable")
	assert.Greater(t, counts[1512], counts[1280], "weights not respected")
}

// TestSeedHash_SaltSeparatesFields different fields under the same seed must
// vary independently, otherwise screen size and chrome height would change in
// lockstep across every account.
func TestSeedHash_SaltSeparatesFields(t *testing.T) {
	assert.NotEqual(t, seedHash(98759, "screen"), seedHash(98759, "chrome"))
	assert.Equal(t, seedHash(98759, "screen"), seedHash(98759, "screen"))
}
