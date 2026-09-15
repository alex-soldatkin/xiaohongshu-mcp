//go:build integration

// Package-level probe harness for the bundled CloakBrowser build.
//
// Purpose (issue #4): record a baseline of every detection-relevant signal the
// production launch actually produces, so that the geometry (#1), timezone
// (#2), flag-coherence (#9), typing (#8) and persistence (#6) work has an
// acceptance gate and so that no flag is added on a guess. A flag that fights
// a patch the binary already applies is worse than no flag at all.
//
// Run:
//
//	GOARCH=arm64 go test -tags integration -run TestProbe -v ./browser/
//
// GOARCH matters: on an arm64 host with `go env GOARCH=amd64`, EnsureBrowser
// fails with "no prebuilt browser for this platform".
package browser

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/xpzouying/headless_browser"
)

// probeSeed pins the fingerprint seed so a rerun is comparable with the
// recorded baseline. Seed-derived signals (WebGL renderer, hardwareConcurrency,
// deviceMemory, canvas hash) change wholesale when this changes.
const probeSeed = 98759

// probeNavTimeout bounds page load; probeEvalTimeout bounds the JS blob, which
// contains a WebRTC gather that can hang when STUN is unreachable.
const (
	probeNavTimeout  = 30 * time.Second
	probeEvalTimeout = 45 * time.Second
)

const probeHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>probe</title></head><body><p>probe</p></body></html>`

// ---------------------------------------------------------------------------
// result shapes
// ---------------------------------------------------------------------------

type fontProbe struct {
	Name    string  `json:"name"`
	Width   float64 `json:"width"`
	Present bool    `json:"present"`
	Check   bool    `json:"check"`
}

type webrtcProbe struct {
	Supported bool     `json:"supported"`
	Reason    string   `json:"reason"`
	Count     int      `json:"count"`
	IPs       []string `json:"ips"`
	Raw       []string `json:"raw"`
	Error     string   `json:"error"`
	// GatheringState / SDPCandidates cross-check the onicecandidate stream.
	GatheringState string `json:"gatheringState"`
	SDPCandidates  int    `json:"sdpCandidates"`
}

type probeResult struct {
	// identity
	Webdriver bool     `json:"webdriver"`
	UserAgent string   `json:"userAgent"`
	Language  string   `json:"language"`
	Languages []string `json:"languages"`
	Platform  string   `json:"platform"`

	// hardware
	HardwareConcurrency int      `json:"hardwareConcurrency"`
	DeviceMemory        *float64 `json:"deviceMemory"`

	// plugin surface
	Plugins          int  `json:"plugins"`
	MimeTypes        int  `json:"mimeTypes"`
	PDFViewerEnabled bool `json:"pdfViewerEnabled"`

	// permission coherence
	NotificationPermission string `json:"notificationPermission"`
	PermissionQueryState   string `json:"permissionQueryState"`

	// window.chrome
	ChromeType      string `json:"chromeType"`
	ChromeLoadTimes string `json:"chromeLoadTimes"`
	ChromeCsi       string `json:"chromeCsi"`
	ChromeRuntime   string `json:"chromeRuntime"`

	// automation leftovers
	AutomationKeys []string `json:"automationKeys"`

	// gpu / canvas
	WebGLVendor   string `json:"webglVendor"`
	WebGLRenderer string `json:"webglRenderer"`
	WebGLVersion  string `json:"webglVersion"`
	CanvasHash    string `json:"canvasHash"`
	CanvasLen     int    `json:"canvasLen"`
	// CanvasMeasureText is the 2d context text width for the font probe string.
	// Real Chrome ~564; this build returns ~0 (see the baseline notes).
	CanvasMeasureText float64 `json:"canvasMeasureText"`

	// geometry
	ScreenW      int     `json:"screenW"`
	ScreenH      int     `json:"screenH"`
	AvailW       int     `json:"availW"`
	AvailH       int     `json:"availH"`
	DPR          float64 `json:"dpr"`
	ColorDepth   int     `json:"colorDepth"`
	InnerW       int     `json:"innerW"`
	InnerH       int     `json:"innerH"`
	OuterW       int     `json:"outerW"`
	OuterH       int     `json:"outerH"`
	GeometrySane bool    `json:"geometrySane"`

	// time / locale
	TZ          string `json:"tz"`
	ICULocale   string `json:"icuLocale"`
	TZOffset    int    `json:"tzOffset"`
	TZOffsetJan int    `json:"tzOffsetJan"`
	TZOffsetJul int    `json:"tzOffsetJul"`

	// fonts
	FontBaseline float64     `json:"fontBaseline"`
	Fonts        []fontProbe `json:"fonts"`

	// network
	WebRTC webrtcProbe `json:"webrtc"`

	// storage
	LocalStorageOK string `json:"localStorageOK"`
}

// ---------------------------------------------------------------------------
// the probe blob: one async function, one JSON string back
// ---------------------------------------------------------------------------
//
// Deliberately one evaluation rather than many: every signal is then read from
// a single renderer state, so nothing can drift between reads (geometry in
// particular changes if anything resizes the window mid-run).
//
// No template literals — the Go source uses raw string quoting.
const probeJS = `async () => {
  const out = {};
  const nav = navigator;

  out.webdriver = nav.webdriver === true;
  out.userAgent = nav.userAgent;
  out.language = nav.language;
  out.languages = Array.prototype.slice.call(nav.languages || []);
  out.platform = nav.platform;
  out.hardwareConcurrency = nav.hardwareConcurrency;
  out.deviceMemory = (nav.deviceMemory === undefined) ? null : nav.deviceMemory;
  out.plugins = nav.plugins ? nav.plugins.length : -1;
  out.mimeTypes = nav.mimeTypes ? nav.mimeTypes.length : -1;
  out.pdfViewerEnabled = nav.pdfViewerEnabled === true;

  try { out.notificationPermission = Notification.permission; }
  catch (e) { out.notificationPermission = 'ERR: ' + e.message; }
  try {
    const st = await nav.permissions.query({ name: 'notifications' });
    out.permissionQueryState = st.state;
  } catch (e) { out.permissionQueryState = 'ERR: ' + e.message; }

  const ch = window.chrome;
  out.chromeType = typeof ch;
  out.chromeLoadTimes = typeof (ch && ch.loadTimes);
  out.chromeCsi = typeof (ch && ch.csi);
  out.chromeRuntime = typeof (ch && ch.runtime);

  const re = /cdc_|\$cdc|selenium|webdriver|driver_|_phantom|domAutomation|_Selenium|callSelenium|fxdriver/i;
  const hits = [];
  const scan = (obj, label) => {
    try {
      for (const k of Object.getOwnPropertyNames(obj)) {
        if (re.test(k)) hits.push(label + '.' + k);
      }
    } catch (e) { hits.push(label + ':ERR'); }
  };
  scan(window, 'window');
  scan(document, 'document');
  try {
    for (const a of document.documentElement.getAttributeNames()) {
      if (re.test(a)) hits.push('html[' + a + ']');
    }
  } catch (e) {}
  out.automationKeys = hits;

  try {
    const c = document.createElement('canvas');
    const gl = c.getContext('webgl') || c.getContext('experimental-webgl');
    const dbg = gl.getExtension('WEBGL_debug_renderer_info');
    out.webglVendor = dbg ? gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL) : 'NO_DEBUG_EXT';
    out.webglRenderer = dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : 'NO_DEBUG_EXT';
    out.webglVersion = gl.getParameter(gl.VERSION);
  } catch (e) {
    out.webglVendor = 'ERR: ' + e.message;
    out.webglRenderer = '';
    out.webglVersion = '';
  }

  try {
    const c = document.createElement('canvas');
    c.width = 300; c.height = 60;
    const x = c.getContext('2d');
    x.textBaseline = 'top';
    x.font = '14px Arial';
    x.fillStyle = '#f60';
    x.fillRect(10, 10, 120, 30);
    x.fillStyle = '#069';
    x.fillText('xhs-mcp probe 你好 ☺', 12, 20);
    const d = c.toDataURL();
    let h = 5381;
    for (let i = 0; i < d.length; i++) { h = ((h * 33) ^ d.charCodeAt(i)) >>> 0; }
    out.canvasHash = h.toString(16);
    out.canvasLen = d.length;
  } catch (e) { out.canvasHash = 'ERR: ' + e.message; out.canvasLen = 0; }

  out.screenW = screen.width;
  out.screenH = screen.height;
  out.availW = screen.availWidth;
  out.availH = screen.availHeight;
  out.dpr = window.devicePixelRatio;
  out.colorDepth = screen.colorDepth;
  out.innerW = window.innerWidth;
  out.innerH = window.innerHeight;
  out.outerW = window.outerWidth;
  out.outerH = window.outerHeight;
  out.geometrySane =
    window.innerWidth <= screen.width &&
    window.innerHeight <= screen.height &&
    window.outerWidth >= window.innerWidth;

  const ro = Intl.DateTimeFormat().resolvedOptions();
  out.tz = ro.timeZone;
  out.icuLocale = ro.locale;
  out.tzOffset = new Date().getTimezoneOffset();
  out.tzOffsetJan = new Date(2025, 0, 15).getTimezoneOffset();
  out.tzOffsetJul = new Date(2025, 6, 15).getTimezoneOffset();

  // Font probe: rendered width of a wide/narrow mix at 72px against the
  // monospace fallback. Equal width == the named family was not resolved.
  //
  // Measured through the DOM (offsetWidth), NOT canvas measureText: this build
  // returns ~0 from measureText (see canvasMeasureText below), so the canvas
  // route cannot distinguish any two fonts.
  try {
    const FONTS = ['Arial', 'Helvetica', 'SimSun', 'Microsoft YaHei',
                   'PingFang SC', 'Times New Roman', 'WenQuanYi Zen Hei'];
    const probe = 'mmmmmmmmmmlli';
    const s = document.createElement('span');
    s.textContent = probe;
    s.style.position = 'absolute';
    s.style.left = '-9999px';
    s.style.whiteSpace = 'nowrap';
    document.body.appendChild(s);
    s.style.font = '72px monospace';
    const base = s.getBoundingClientRect().width;
    out.fontBaseline = base;
    out.fonts = FONTS.map(f => {
      s.style.font = '72px "' + f + '", monospace';
      const w = s.getBoundingClientRect().width;
      let chk = false;
      try { chk = document.fonts.check('72px "' + f + '"'); } catch (e) {}
      return { name: f, width: w, present: w !== base, check: chk };
    });
    s.remove();
  } catch (e) { out.fonts = []; out.fontBaseline = -1; }

  // Canvas text metrics. Real Chrome returns ~564 for this string at 72px
  // monospace; this build returns a near-zero float for every string, so
  // measureText carries no information and is itself an anomaly.
  try {
    const x = document.createElement('canvas').getContext('2d');
    x.font = '72px monospace';
    out.canvasMeasureText = x.measureText('mmmmmmmmmmlli').width;
  } catch (e) { out.canvasMeasureText = -1; }

  try {
    localStorage.setItem('__probe_rw', '1');
    out.localStorageOK = localStorage.getItem('__probe_rw') === '1' ? 'yes' : 'no';
    localStorage.removeItem('__probe_rw');
  } catch (e) { out.localStorageOK = 'ERR: ' + e.message; }

  // WebRTC: does a host IP leak past the proxy? Time-boxed — ICE gathering
  // never completes when STUN is unreachable (offline, or a proxy that drops
  // UDP), and there is no event for "gave up".
  out.webrtc = await (async () => {
    try {
      if (typeof RTCPeerConnection === 'undefined') return { supported: false, reason: 'no RTCPeerConnection' };
      const pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
      const cands = [];
      const settled = new Promise(res => {
        const t = setTimeout(() => res('timeout'), 8000);
        pc.onicecandidate = e => {
          if (!e.candidate) { clearTimeout(t); res('gathering-complete'); return; }
          cands.push(e.candidate.candidate);
        };
      });
      pc.createDataChannel('probe');
      await pc.setLocalDescription(await pc.createOffer());
      const reason = await settled;
      const state = pc.iceGatheringState;
      try { pc.close(); } catch (e) {}
      const seen = [];
      for (const c of cands) {
        const p = c.split(' ');
        const entry = p[4] + ' (' + p[7] + ')';
        if (seen.indexOf(entry) < 0) seen.push(entry);
      }
      // Cross-check the event stream against the SDP: a candidate can land in
      // the offer without ever firing onicecandidate.
      let sdpCands = [];
      try { sdpCands = (pc.localDescription.sdp.match(/a=candidate:[^\r\n]*/g) || []); } catch (e) {}
      return {
        supported: true, reason: reason, count: cands.length, ips: seen, raw: cands,
        gatheringState: state, sdpCandidates: sdpCands.length
      };
    } catch (e) {
      return { supported: true, reason: 'error', error: e.message, ips: [] };
    }
  })();

  return JSON.stringify(out);
}`

// ---------------------------------------------------------------------------
// launch helpers
// ---------------------------------------------------------------------------

// launchProbeBrowser launches with the EXACT production option set from
// buildOptions, optionally with extra flags merged on top.
//
// WithExtraFlags replaces the whole map, and the last option applied wins, so
// appending a merged map after buildOptions is the correct way to add a flag
// without losing fingerprint-brand.
func launchProbeBrowser(t *testing.T, extra map[string]string) *headless_browser.Browser {
	t.Helper()

	bin, err := EnsureBrowser()
	if err != nil {
		t.Skipf("SKIP: bundled browser unavailable (set GOARCH=arm64 on an arm64 host): %v", err)
	}

	cfg := newConfig(true, WithFingerprintSeed(probeSeed))
	cfg.binPath = bin
	// Cookies deliberately not seeded: the probe must not depend on a logged-in
	// session, and cookies do not touch any signal measured here.

	opts := buildOptions(cfg)
	if len(extra) > 0 {
		flags := launchFlags(cfg)
		for k, v := range extra {
			flags[k] = v
		}
		opts = append(opts, headless_browser.WithExtraFlags(flags))
	}
	return headless_browser.New(opts...)
}

// probeServer serves the probe page from a fixed loopback origin. A fixed
// origin matters for the persistence sub-test: localStorage is keyed per
// origin, so run 1 and run 2 must hit the same host:port.
func probeServer(t *testing.T) (string, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(probeHTML))
	})}
	go func() { _ = srv.Serve(ln) }()

	return "http://" + ln.Addr().String() + "/", func() { _ = srv.Close() }
}

func openProbePage(t *testing.T, b *headless_browser.Browser, url string) *rod.Page {
	t.Helper()

	page := b.NewPage()
	page = page.Timeout(probeNavTimeout)
	page.MustNavigate(url).MustWaitLoad()
	return page.CancelTimeout()
}

func runProbe(t *testing.T, page *rod.Page) probeResult {
	t.Helper()

	raw := page.Timeout(probeEvalTimeout).MustEval(probeJS).Str()

	var res probeResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("decode probe result: %v\nraw: %s", err, raw)
	}
	return res
}

// ---------------------------------------------------------------------------
// table rendering
// ---------------------------------------------------------------------------

type row struct{ signal, value, verdict string }

// verdict labels. CLEAN = safe as-is; BROKEN = a detectable contradiction with
// an open issue; UNKNOWN = measured but no reference to judge it against.
const (
	vClean   = "CLEAN"
	vBroken  = "BROKEN"
	vUnknown = "UNKNOWN"
	vInfo    = "INFO"
)

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func buildRows(r probeResult) []row {
	rows := []row{}
	add := func(s, v, verdict string) { rows = append(rows, row{s, v, verdict}) }

	// --- identity -----------------------------------------------------------
	wd := vClean
	if r.Webdriver {
		wd = vBroken
	}
	add("navigator.webdriver", fmt.Sprintf("%v", r.Webdriver), wd+" (false expected; binary neutralises --enable-automation)")

	ua := vClean
	if strings.Contains(r.UserAgent, "HeadlessChrome") {
		ua = vBroken
	}
	add("navigator.userAgent", r.UserAgent, ua)
	add("navigator.platform", r.Platform, vInfo)

	langV := vClean
	if r.Language != "zh-CN" {
		langV = vBroken
	}
	add("navigator.language", r.Language, langV+" (WithLanguage=zh-CN)")
	add("navigator.languages", strings.Join(r.Languages, ","), langV)

	// --- hardware -----------------------------------------------------------
	add("hardwareConcurrency", fmt.Sprintf("%d", r.HardwareConcurrency), vInfo+" (seed-derived)")
	dm := "undefined"
	if r.DeviceMemory != nil {
		dm = fmt.Sprintf("%g", *r.DeviceMemory)
	}
	add("deviceMemory", dm, vInfo+" (seed-derived)")

	// --- plugin surface -----------------------------------------------------
	pl := vClean
	if r.Plugins == 0 || r.MimeTypes == 0 {
		pl = vBroken
	}
	add("plugins / mimeTypes", fmt.Sprintf("%d / %d", r.Plugins, r.MimeTypes), pl+" (5/2 = real Chrome PDF set)")
	add("pdfViewerEnabled", yesNo(r.PDFViewerEnabled), vInfo)

	// --- permission coherence ----------------------------------------------
	// The classic headless tell: Notification.permission 'denied' while
	// permissions.query reports 'prompt'.
	perm := vClean
	if r.NotificationPermission == "denied" && r.PermissionQueryState == "prompt" {
		perm = vBroken
	}
	add("Notification.permission", r.NotificationPermission, perm)
	add("permissions.query(notifications)", r.PermissionQueryState, perm+" (must agree with the line above)")

	// --- window.chrome ------------------------------------------------------
	ck := vClean
	if r.ChromeLoadTimes != "function" || r.ChromeCsi != "function" {
		ck = vBroken
	}
	add("window.chrome", r.ChromeType, ck)
	add("chrome.loadTimes / chrome.csi", r.ChromeLoadTimes+" / "+r.ChromeCsi, ck)
	add("chrome.runtime", r.ChromeRuntime, vInfo+" (object only inside an extension context)")

	// --- automation leftovers ----------------------------------------------
	ak := vClean
	akv := "none"
	if len(r.AutomationKeys) > 0 {
		ak = vBroken
		akv = strings.Join(r.AutomationKeys, ", ")
	}
	add("cdc_/$cdc/selenium keys", akv, ak+" (window + document + html attrs)")

	// --- gpu / canvas -------------------------------------------------------
	add("WebGL UNMASKED_VENDOR", r.WebGLVendor, vInfo+" (seed-derived)")
	add("WebGL UNMASKED_RENDERER", r.WebGLRenderer, vInfo+" (seed-derived)")
	add("WebGL VERSION", r.WebGLVersion, vInfo)
	add("canvas hash (djb2 of dataURL)", fmt.Sprintf("%s (len %d)", r.CanvasHash, r.CanvasLen), vInfo+" (stable per seed)")
	// Not tracked by any issue yet: the binary zeroes canvas text metrics.
	cm := vInfo
	if r.CanvasMeasureText > -1 && r.CanvasMeasureText < 1 {
		cm = vBroken + " (real Chrome ~564; always ~0 here, untracked)"
	}
	add("canvas measureText width (72px mono)", fmt.Sprintf("%.6f px", r.CanvasMeasureText), cm)

	// --- geometry -----------------------------------------------------------
	geo := vBroken
	if r.GeometrySane {
		geo = vClean
	}
	add("screen w x h", fmt.Sprintf("%d x %d", r.ScreenW, r.ScreenH), geo)
	add("screen avail w x h", fmt.Sprintf("%d x %d", r.AvailW, r.AvailH), geo)
	add("devicePixelRatio", fmt.Sprintf("%g", r.DPR), geo)
	add("colorDepth", fmt.Sprintf("%d", r.ColorDepth), vInfo)
	add("inner w x h", fmt.Sprintf("%d x %d", r.InnerW, r.InnerH), geo)
	add("outer w x h", fmt.Sprintf("%d x %d", r.OuterW, r.OuterH), geo)
	add("geometrySane", yesNo(r.GeometrySane),
		geo+" (inner<=screen && outer>=inner) -- issue #1")

	// --- time / locale ------------------------------------------------------
	tzV := vBroken
	if r.TZ == "Asia/Shanghai" {
		tzV = vClean
	}
	add("Intl timeZone", r.TZ, tzV+" (want Asia/Shanghai; leaks from host) -- issue #2")
	locV := vBroken
	if strings.HasPrefix(r.ICULocale, "zh") {
		locV = vClean
	}
	add("Intl locale (ICU)", r.ICULocale, locV+" (want zh-CN; --lang never set) -- issue #9")
	add("Date.getTimezoneOffset()", fmt.Sprintf("%d min", r.TZOffset),
		tzV+" (must track Intl timeZone; -480 == Asia/Shanghai)")
	add("getTimezoneOffset Jan / Jul", fmt.Sprintf("%d / %d", r.TZOffsetJan, r.TZOffsetJul),
		vInfo+" (differ => DST zone; Asia/Shanghai has none)")

	// --- fonts --------------------------------------------------------------
	add("font fallback baseline (72px monospace)", fmt.Sprintf("%.2f px", r.FontBaseline), vInfo+" (DOM width, not canvas)")
	for _, f := range r.Fonts {
		v := vInfo
		if f.Present {
			v = "present"
		} else {
			v = "ABSENT (falls back)"
		}
		add("font: "+f.Name,
			fmt.Sprintf("%.2f px (fonts.check=%v)", f.Width, f.Check), v)
	}

	// --- webrtc -------------------------------------------------------------
	if !r.WebRTC.Supported {
		add("WebRTC", "RTCPeerConnection unavailable", vClean+" (nothing to leak)")
	} else if r.WebRTC.Error != "" {
		add("WebRTC", "error: "+r.WebRTC.Error, vUnknown)
	} else {
		ips := "none"
		if len(r.WebRTC.IPs) > 0 {
			ips = strings.Join(r.WebRTC.IPs, ", ")
		}
		v := vUnknown
		switch {
		case r.WebRTC.Count == 0 && r.WebRTC.SDPCandidates == 0:
			v = vClean + " (no candidates at all -- nothing to leak)"
		case hasHostCandidate(r.WebRTC.IPs):
			v = vBroken + " (host candidate present) -- issue #9"
		default:
			v = vUnknown + " (srflx only; compare against proxy egress) -- issue #9"
		}
		add("WebRTC ICE candidates", fmt.Sprintf("%d events, %d in SDP (%s, state=%s)",
			r.WebRTC.Count, r.WebRTC.SDPCandidates, r.WebRTC.Reason, r.WebRTC.GatheringState), v)
		add("WebRTC candidate IPs", ips, v)
	}

	// --- storage ------------------------------------------------------------
	add("localStorage read/write", r.LocalStorageOK, vInfo+" (survival across relaunch: issue #6)")

	return rows
}

// hasHostCandidate reports whether any ICE candidate is of type host, i.e. a
// local interface address rather than a proxy/STUN-reflected one.
func hasHostCandidate(ips []string) bool {
	for _, s := range ips {
		if strings.Contains(s, "(host)") && !strings.Contains(s, ".local") {
			return true
		}
	}
	return false
}

func printTable(title string, rows []row) {
	fmt.Printf("\n%s\n%s\n", title, strings.Repeat("=", len(title)))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SIGNAL\tVALUE\tVERDICT")
	fmt.Fprintf(w, "%s\t%s\t%s\n", strings.Repeat("-", 34), strings.Repeat("-", 60), strings.Repeat("-", 24))
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.signal, truncate(r.value, 88), r.verdict)
	}
	_ = w.Flush()
	fmt.Println()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestProbeBaseline records the full detection baseline. It asserts only where
// a reference measurement exists; everything else is printed, not judged.
// Keeping assertions loose is deliberate — a red test on an unknown signal
// teaches nothing, and the point of this harness is to be the gate for the
// fixes, not to encode guesses.
func TestProbeBaseline(t *testing.T) {
	url, stop := probeServer(t)
	defer stop()

	b := launchProbeBrowser(t, nil)
	defer b.Close()

	page := openProbePage(t, b, url)
	res := runProbe(t, page)

	printTable(fmt.Sprintf("DETECTION BASELINE  (seed=%d, headless=true, origin=%s)", probeSeed, url),
		buildRows(res))

	// --- assertions that a reference measurement supports -------------------

	if res.Webdriver {
		t.Errorf("navigator.webdriver is true; the bundled binary is expected to neutralise --enable-automation")
	}
	if strings.Contains(res.UserAgent, "HeadlessChrome") {
		t.Errorf("userAgent leaks HeadlessChrome: %s", res.UserAgent)
	}
	if len(res.AutomationKeys) > 0 {
		t.Errorf("automation keys present on window/document: %v", res.AutomationKeys)
	}
	if res.Language != "zh-CN" {
		t.Errorf("navigator.language = %q, want zh-CN (WithLanguage)", res.Language)
	}
	if res.Plugins == 0 || res.MimeTypes == 0 {
		t.Errorf("empty plugin surface: plugins=%d mimeTypes=%d", res.Plugins, res.MimeTypes)
	}
	if res.NotificationPermission == "denied" && res.PermissionQueryState == "prompt" {
		t.Errorf("classic headless mismatch: Notification.permission=denied vs query state=prompt")
	}
	if res.ChromeLoadTimes != "function" || res.ChromeCsi != "function" {
		t.Errorf("window.chrome shims missing: loadTimes=%s csi=%s", res.ChromeLoadTimes, res.ChromeCsi)
	}

	// --- fixed, now gated ---------------------------------------------------
	// These were the known-broken lines of the original baseline. Each fix has
	// landed and is measured, so they are assertions rather than log lines.
	if !res.GeometrySane {
		t.Errorf("#1: geometry impossible -- screen %dx%d, inner %dx%d, outer %dx%d, dpr %g",
			res.ScreenW, res.ScreenH, res.InnerW, res.InnerH, res.OuterW, res.OuterH, res.DPR)
	}
	if wantDPR := 2.0; runtime.GOOS == "darwin" && res.DPR != wantDPR {
		t.Errorf("#1: devicePixelRatio = %g on a macOS fingerprint, want %g", res.DPR, wantDPR)
	}
	if res.TZ != "Asia/Shanghai" {
		t.Errorf("#2: Intl timeZone = %q, want Asia/Shanghai", res.TZ)
	}
	// Intl and Date must agree: spoofing that moves one and not the other is
	// itself the detection. Asia/Shanghai is UTC+8 with no DST, so -480 all year.
	if res.TZOffset != -480 || res.TZOffsetJan != -480 || res.TZOffsetJul != -480 {
		t.Errorf("#2: getTimezoneOffset = %d (jan %d, jul %d), want -480 everywhere for Asia/Shanghai",
			res.TZOffset, res.TZOffsetJan, res.TZOffsetJul)
	}
	if res.ICULocale != res.Language {
		t.Errorf("#9: ICU locale %q disagrees with navigator.language %q", res.ICULocale, res.Language)
	}

	// Still open, logged rather than failed.
	if hasHostCandidate(res.WebRTC.IPs) {
		t.Logf("KNOWN (#9): WebRTC host candidates leaked: %v", res.WebRTC.IPs)
	}
	if res.AvailH == res.ScreenH {
		t.Logf("KNOWN (#1, accepted): screen.availHeight == screen.height (%d); no flag or CDP call moves the work area on this build", res.AvailH)
	}
}

// TestProbeGeometryPerSeed is the seed-stability half of issue #1: geometry is
// part of the account's identity, so it must be identical across restarts of
// the same account and different between accounts. A hard-coded window would
// pass the sanity check above and still make every account look like the same
// machine.
func TestProbeGeometryPerSeed(t *testing.T) {
	url, stop := probeServer(t)
	defer stop()

	measure := func(seed int) probeResult {
		bin, err := EnsureBrowser()
		if err != nil {
			t.Skipf("SKIP: bundled browser unavailable: %v", err)
		}
		cfg := newConfig(true, WithFingerprintSeed(seed))
		cfg.binPath = bin

		b := headless_browser.New(buildOptions(cfg)...)
		defer b.Close()
		return runProbe(t, openProbePage(t, b, url))
	}

	type box struct{ sw, sh, iw, ih, ow, oh int }
	of := func(r probeResult) box {
		return box{r.ScreenW, r.ScreenH, r.InnerW, r.InnerH, r.OuterW, r.OuterH}
	}

	first := of(measure(probeSeed))
	second := of(measure(probeSeed))
	other := of(measure(probeSeed + 7919))

	t.Logf("seed %d: %+v / relaunch %+v ; seed %d: %+v", probeSeed, first, second, probeSeed+7919, other)

	if first != second {
		t.Errorf("#1: same seed gave different geometry across launches: %+v vs %+v", first, second)
	}
	if first == other {
		t.Errorf("#1: two different seeds share one geometry %+v", first)
	}
}

// TestProbeProfilePersistence is the acceptance gate for issue #6.
//
// KNOWN FAILING. It is expected to fail until a persistent profile lands.
// headless_browser has no WithUserDataDir, and rod's launcher.Cleanup() does an
// unconditional os.RemoveAll(UserDataDir) inside Browser.Close(), so the
// profile written by run 1 is deleted before run 2 can open it. That is exactly
// what #6 (and its dependency #12) is about. The failure is real and must not
// be papered over: do not relax it, delete it when the fix lands.
func TestProbeProfilePersistence(t *testing.T) {
	url, stop := probeServer(t)
	defer stop()

	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	want := fmt.Sprintf("b1-%d", time.Now().UnixNano())

	// run 1: write a localStorage key, then close as production does.
	func() {
		b := launchProbeBrowser(t, map[string]string{"user-data-dir": dir})
		defer b.Close()

		page := openProbePage(t, b, url)
		page.MustEval(`(v) => localStorage.setItem('__probe_b1', v)`, want)
		got := page.MustEval(`() => localStorage.getItem('__probe_b1')`).Str()
		if got != want {
			t.Fatalf("run 1: localStorage write did not stick: got %q", got)
		}
		t.Logf("run 1: wrote __probe_b1=%s into profile %s", want, dir)
	}()

	entries, err := os.ReadDir(dir)
	profileSurvived := err == nil && len(entries) > 0
	t.Logf("after run 1 Close(): profile dir exists with content = %v (readdir err: %v)", profileSurvived, err)

	// run 2: same profile dir, same origin.
	b := launchProbeBrowser(t, map[string]string{"user-data-dir": dir})
	defer b.Close()

	page := openProbePage(t, b, url)
	got := page.MustEval(`() => localStorage.getItem('__probe_b1') || ''`).Str()

	printTable("PROFILE PERSISTENCE  (acceptance gate for issue #6)", []row{
		{"user-data-dir", dir, vInfo},
		{"profile dir survives Close()", yesNo(profileSurvived), map[bool]string{true: vClean, false: vBroken + " (launcher.Cleanup removes it)"}[profileSurvived]},
		{"localStorage written in run 1", want, vInfo},
		{"localStorage read in run 2", fmt.Sprintf("%q", got), map[bool]string{true: vClean, false: vBroken}[got == want]},
	})

	if got != want {
		t.Errorf("KNOWN FAILING acceptance gate for #6: localStorage did not survive the relaunch; "+
			"run 2 read %q, want %q. Cause: headless_browser has no WithUserDataDir and "+
			"rod launcher.Cleanup() unconditionally removes the profile in Close().", got, want)
	}
}

// TestProbeIMESetComposition answers the open question in issue #8: can the
// typing rewrite use real composition events (Input.imeSetComposition) instead
// of simulating an IME with keyCode 229 keydown/keyup pairs?
func TestProbeIMESetComposition(t *testing.T) {
	url, stop := probeServer(t)
	defer stop()

	b := launchProbeBrowser(t, nil)
	defer b.Close()

	page := openProbePage(t, b, url)

	page.MustSetDocumentContent(`<!doctype html><html><body>` +
		`<input id="a"><div id="b" contenteditable="true"></div></body></html>`)
	page.MustEval(`() => {
		window.__ev = [];
		for (const t of ['compositionstart','compositionupdate','compositionend','beforeinput','input','keydown','keyup']) {
			document.addEventListener(t, e => window.__ev.push(t), true);
		}
		document.getElementById('a').focus();
		return true;
	}`)

	callErr := ""
	if err := (proto.InputImeSetComposition{
		Text:           "ni hao",
		SelectionStart: 6,
		SelectionEnd:   6,
	}).Call(page); err != nil {
		callErr = err.Error()
	}

	evs := page.MustEval(`() => JSON.stringify(window.__ev)`).Str()
	value := page.MustEval(`() => document.getElementById('a').value`).Str()

	// Commit, which is what a real IME does at the end of composition.
	commitErr := ""
	if callErr == "" {
		if err := (proto.InputInsertText{Text: "你好"}).Call(page); err != nil {
			commitErr = err.Error()
		}
	}
	evsAfter := page.MustEval(`() => JSON.stringify(window.__ev)`).Str()
	valueAfter := page.MustEval(`() => document.getElementById('a').value`).Str()

	supported := callErr == ""
	verdict := vClean + " (use real composition events in #8)"
	if !supported {
		verdict = vBroken + " (fall back to keyCode 229 simulation in #8)"
	} else if !strings.Contains(evs, "compositionstart") {
		verdict = vUnknown + " (call accepted but no composition events fired)"
	}

	errCell := callErr
	if errCell == "" {
		errCell = "(none)"
	}
	printTable("Input.imeSetComposition SUPPORT  (question raised by issue #8)", []row{
		{"CDP call accepted", yesNo(supported), verdict},
		{"CDP error", errCell, vInfo},
		{"events after setComposition", evs, vInfo},
		{"input.value after setComposition", fmt.Sprintf("%q", value), vInfo},
		{"events after insertText commit", evsAfter, vInfo},
		{"input.value after commit", fmt.Sprintf("%q", valueAfter), vInfo},
		{"insertText error", orNone(commitErr), vInfo},
	})

	// Informational only: this test records an answer, it does not gate.
	t.Logf("Input.imeSetComposition supported=%v err=%q events=%s", supported, callErr, evs)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// sortedKeys keeps map dumps deterministic in the printed tables.
func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// TestProbeLaunchFlagsTable prints the exact production flag set the tables
// above were measured under, so a pasted baseline is self-describing.
func TestProbeLaunchFlagsTable(t *testing.T) {
	cfg := newConfig(true, WithFingerprintSeed(probeSeed))
	flags := launchFlags(cfg)

	rows := []row{{"headless", "true", vInfo}, {"fingerprint (seed)", fmt.Sprintf("%d", probeSeed), vInfo},
		{"fingerprint-platform", "auto (darwin -> macos, else windows)", vInfo},
		{"language", launchLanguage, vInfo}, {"stealth JS", "disabled", vInfo}}
	for _, k := range sortedKeys(flags) {
		rows = append(rows, row{"--" + k, flags[k], vInfo})
	}
	printTable("PRODUCTION LAUNCH FLAGS (browser.launchFlags / browser.buildOptions)", rows)
}
