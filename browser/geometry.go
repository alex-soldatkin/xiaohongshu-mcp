package browser

import (
	"hash/fnv"
	"runtime"
	"strconv"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Window geometry (issue #1).
//
// Headless Chrome reports an 800x600 screen while rod hands the page a
// 1280x800 viewport, so navigator sees innerWidth > screen.width — a viewport
// larger than the display, which cannot happen on real hardware. Neither lever
// fixes that alone: --window-size moves only outerWidth/outerHeight, and
// Emulation.setDeviceMetricsOverride moves only screen.*, devicePixelRatio and
// the viewport. Both were measured, and the fix uses both.
//
// The numbers are derived from the fingerprint seed rather than hard-coded, so
// one account keeps one monitor across restarts (same rationale as
// configs/seed.go) and two accounts do not share a screen size.

// screenProfile is one row of the weighted resolution table. Sizes are CSS
// pixels — what screen.width/height report — not physical panel pixels, so a
// scaled display is listed at its scaled size with the matching dpr.
type screenProfile struct {
	w, h   int
	dpr    float64
	weight int
}

// macScreens: built-in Retina panels at their default "looks like" scaling,
// which is what a Mac reports in CSS pixels. Every entry is dpr 2 because a
// non-Retina Mac is effectively extinct and the fingerprint claims MacIntel.
var macScreens = []screenProfile{
	{w: 1512, h: 982, dpr: 2, weight: 30},  // MacBook Pro 14"
	{w: 1440, h: 900, dpr: 2, weight: 28},  // MacBook Air 13" / older Pro 13"
	{w: 1728, h: 1117, dpr: 2, weight: 18}, // MacBook Pro 16"
	{w: 1470, h: 956, dpr: 2, weight: 14},  // MacBook Air 15"
	{w: 1280, h: 800, dpr: 2, weight: 10},  // Air 13" at "larger text"
}

// winScreens: the common desktop sizes. dpr 1 except the 125%-scaled 1080p
// laptop, which really does report 1536x864 at dpr 1.25 — listing 1920x1080 at
// 1.25 would imply a 2400x1350 panel that does not exist.
var winScreens = []screenProfile{
	{w: 1920, h: 1080, dpr: 1, weight: 38},
	{w: 1536, h: 864, dpr: 1.25, weight: 20}, // 1080p at 125% scaling
	{w: 1366, h: 768, dpr: 1, weight: 16},
	{w: 2560, h: 1440, dpr: 1, weight: 10},
	{w: 1600, h: 900, dpr: 1, weight: 9},
	{w: 1920, h: 1200, dpr: 1, weight: 7},
}

// geometry is the full window picture handed to Chrome for one account.
//
// It reaches the browser through two different channels, because neither one
// covers everything: --window-size drives outerWidth/outerHeight, while
// Emulation.setDeviceMetricsOverride drives screen.*, devicePixelRatio and the
// viewport. Using only the second leaves outer at the headless default of
// 756x556, i.e. a window narrower than its own content (measured).
type geometry struct {
	screenW, screenH int
	availW, availH   int
	outerW, outerH   int
	innerW, innerH   int
	dpr              float64
}

// chromeHeightRange is the vertical space Chrome's own UI takes: tab strip plus
// omnibox, plus a bookmarks bar for some users. Roughly 87-131 CSS px on a
// desktop build, and it is what makes innerHeight < outerHeight.
const (
	chromeHeightMin = 87
	chromeHeightMax = 131
)

// deriveGeometry picks a stable screen for a seed. platform is the resolved
// fingerprint platform ("macos" or "windows"), so the display matches the OS
// the fingerprint claims to be.
//
// seed <= 0 means the fingerprint itself is randomised per launch, and there is
// no identity to stay consistent with; the derivation still runs, it just runs
// off 0 and yields the table's most common entry.
func deriveGeometry(seed int, platform string) geometry {
	table := winScreens
	if platform == "macos" {
		table = macScreens
	}

	p := pickScreen(table, seedHash(seed, "screen"))

	// The work area: the screen minus the strip the OS keeps for itself, the
	// macOS menu bar (usually plus the Dock) or the Windows taskbar. This is
	// the size a maximised window gets, and it is what --window-size is set to.
	//
	// Caveat, measured: screen.availHeight still reports screen.height. Nothing
	// tried moves it — not the CDP override, which has no avail parameter, and
	// not --screen-info, which kills the launch on this build. Patching it from
	// JS would mean redefining a getter on Screen.prototype, which is a louder
	// tell than the equality it would hide. Left as is, deliberately.
	reserved := 40 // Windows taskbar
	if platform == "macos" {
		reserved = 25 + int(seedHash(seed, "dock")%50) // menu bar, plus the Dock when it is not hidden
	}

	g := geometry{
		screenW: p.w,
		screenH: p.h,
		availW:  p.w,
		availH:  p.h - reserved,
		dpr:     p.dpr,
	}

	// A maximised window: it fills the work area, and Chrome's own UI eats the
	// top of it, so the viewport is shorter than the window.
	chrome := chromeHeightMin + int(seedHash(seed, "chrome")%uint64(chromeHeightMax-chromeHeightMin+1))
	g.outerW, g.outerH = g.availW, g.availH
	g.innerW = g.outerW
	g.innerH = g.outerH - chrome

	return g
}

// pickScreen walks the weighted table. Weights are relative, so the table can
// be edited without keeping a running total in sync.
func pickScreen(table []screenProfile, h uint64) screenProfile {
	total := 0
	for _, p := range table {
		total += p.weight
	}

	n := int(h % uint64(total))
	for _, p := range table {
		if n < p.weight {
			return p
		}
		n -= p.weight
	}
	return table[0]
}

// seedHash mixes the seed with a per-field salt so that two fields derived from
// the same seed are independent: without the salt, screen choice and chrome
// height would move together across accounts.
func seedHash(seed int, salt string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	var b [8]byte
	u := uint64(seed)
	for i := range b {
		b[i] = byte(u >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// resolvePlatform mirrors headless_browser's automatic platform choice for an
// empty WithFingerprint value: darwin -> macos, everything else -> windows.
// Duplicated rather than imported because the dependency does not export it;
// if the two ever disagree the probe's dpr assertion catches it.
func resolvePlatform() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return "windows"
}

// windowSizeFlag renders the --window-size value. It is the only way measured
// to move outerWidth/outerHeight; the CDP override does not touch them.
func windowSizeFlag(g geometry) string {
	return strconv.Itoa(g.outerW) + "," + strconv.Itoa(g.outerH)
}

// applyGeometry sets the window metrics on one page.
//
// Emulation.setDeviceMetricsOverride is the only call measured to move
// screen.width/height; Width/Height set the viewport and ScreenWidth/
// ScreenHeight set the display behind it.
func applyGeometry(page *rod.Page, g geometry) error {
	sw, sh := g.screenW, g.screenH
	return proto.EmulationSetDeviceMetricsOverride{
		Width:             g.innerW,
		Height:            g.innerH,
		DeviceScaleFactor: g.dpr,
		Mobile:            false,
		ScreenWidth:       &sw,
		ScreenHeight:      &sh,
	}.Call(page)
}
