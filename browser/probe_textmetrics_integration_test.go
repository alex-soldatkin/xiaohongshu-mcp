//go:build integration

// Acceptance gate for the canvas text-metrics repair (issue #15).
//
// The bundled build multiplies every TextMetrics field by a seed-derived
// near-zero factor, so measureText carries no information and every string
// measures ~0. browser/textmetrics.go calibrates that factor on about:blank and
// injects a shim that undoes it. This file measures both sides of that in one
// launch: a page taken straight off the rod browser never runs the page hook
// and is therefore unshimmed, while a page from NewPage is what production
// gets.
//
// Run:
//
//	GOARCH=arm64 go test -tags integration -run TestProbeTextMetrics -v ./browser/
package browser

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/go-rod/rod"
	"github.com/xpzouying/headless_browser"
)

// textMetricsJS measures one page: a matrix of fonts, strings and sizes
// through canvas, the same strings through DOM layout for a reference, and the
// stringification of everything the shim touches.
const textMetricsJS = `() => {
  const out = {samples: [], meta: {}};
  const ctx = document.createElement('canvas').getContext('2d');

  const span = document.createElement('span');
  span.style.cssText = 'position:absolute;left:-9999px;top:-9999px;white-space:nowrap';
  document.body.appendChild(span);

  const fonts = ['72px monospace', '72px Arial', '72px "Times New Roman"',
                 '72px "PingFang SC"', '16px monospace', '32px monospace'];
  const texts = ['i', 'mmmmmmmmmmlli', 'The quick brown fox jumps over the lazy dog'];

  for (const f of fonts) {
    for (const t of texts) {
      ctx.font = f;
      span.style.font = f;
      span.textContent = t;
      const m = ctx.measureText(t);
      out.samples.push({
        font: f, text: t, len: t.length,
        width: m.width,
        dom: span.getBoundingClientRect().width,
        abbL: m.actualBoundingBoxLeft, abbR: m.actualBoundingBoxRight,
        abbA: m.actualBoundingBoxAscent, abbD: m.actualBoundingBoxDescent,
        fbA: m.fontBoundingBoxAscent, fbD: m.fontBoundingBoxDescent,
        hang: m.hangingBaseline, alph: m.alphabeticBaseline, ideo: m.ideographicBaseline
      });
    }
  }
  span.remove();

  // Determinism: real metrics never move between calls.
  ctx.font = '72px monospace';
  out.meta.repeat = [];
  for (let i = 0; i < 5; i++) out.meta.repeat.push(ctx.measureText('mmmmmmmmmmlli').width);
  out.meta.empty = ctx.measureText('').width;

  // A same-origin iframe and a worker-less OffscreenCanvas: does the repair
  // reach every document the page owns?
  try {
    const f = document.createElement('iframe');
    document.body.appendChild(f);
    const c2 = f.contentDocument.createElement('canvas').getContext('2d');
    c2.font = '72px monospace';
    out.meta.iframe = c2.measureText('mmmmmmmmmmlli').width;
    f.remove();
  } catch (e) { out.meta.iframe = -1; }
  try {
    const oc = new OffscreenCanvas(8, 8).getContext('2d');
    oc.font = '72px monospace';
    out.meta.offscreen = oc.measureText('mmmmmmmmmmlli').width;
  } catch (e) { out.meta.offscreen = -1; }

  // Stringification. measureText must stay byte-identical to a native method;
  // the patched accessors must at least stringify as native code rather than
  // as JS source.
  const mt = CanvasRenderingContext2D.prototype.measureText;
  out.meta.measureTextToString = Function.prototype.toString.call(mt);
  out.meta.measureTextName = mt.name;
  out.meta.measureTextLength = mt.length;
  const wd = Object.getOwnPropertyDescriptor(TextMetrics.prototype, 'width');
  out.meta.widthGetterToString = Function.prototype.toString.call(wd.get);
  out.meta.widthGetterName = wd.get.name;
  out.meta.widthGetterLength = wd.get.length;
  out.meta.widthEnumerable = wd.enumerable;
  out.meta.widthConfigurable = wd.configurable;
  out.meta.widthHasSetter = wd.set !== undefined;
  out.meta.toStringToString = Function.prototype.toString.toString();

  // Object identity of the returned metrics: the shim must not hand back
  // something that fails instanceof or rejects its own getter.
  const m = ctx.measureText('mmmmmmmmmmlli');
  out.meta.instanceOf = m instanceof TextMetrics;
  out.meta.brand = Object.prototype.toString.call(m);
  out.meta.ownProps = Object.getOwnPropertyNames(m).length;
  try { out.meta.getterOnInstance = wd.get.call(m); } catch (e) { out.meta.getterOnInstance = 'ERR ' + e.message; }

  return JSON.stringify(out);
}`

type tmSample struct {
	Font  string  `json:"font"`
	Text  string  `json:"text"`
	Len   int     `json:"len"`
	Width float64 `json:"width"`
	Dom   float64 `json:"dom"`
	AbbL  float64 `json:"abbL"`
	AbbR  float64 `json:"abbR"`
	AbbA  float64 `json:"abbA"`
	AbbD  float64 `json:"abbD"`
	FbA   float64 `json:"fbA"`
	FbD   float64 `json:"fbD"`
	Hang  float64 `json:"hang"`
	Alph  float64 `json:"alph"`
	Ideo  float64 `json:"ideo"`
}

type tmMeta struct {
	Repeat              []float64 `json:"repeat"`
	Empty               float64   `json:"empty"`
	Iframe              float64   `json:"iframe"`
	Offscreen           float64   `json:"offscreen"`
	MeasureTextToString string    `json:"measureTextToString"`
	MeasureTextName     string    `json:"measureTextName"`
	MeasureTextLength   int       `json:"measureTextLength"`
	WidthGetterToString string    `json:"widthGetterToString"`
	WidthGetterName     string    `json:"widthGetterName"`
	WidthGetterLength   int       `json:"widthGetterLength"`
	WidthEnumerable     bool      `json:"widthEnumerable"`
	WidthConfigurable   bool      `json:"widthConfigurable"`
	WidthHasSetter      bool      `json:"widthHasSetter"`
	ToStringToString    string    `json:"toStringToString"`
	InstanceOf          bool      `json:"instanceOf"`
	Brand               string    `json:"brand"`
	OwnProps            int       `json:"ownProps"`
	GetterOnInstance    any       `json:"getterOnInstance"`
}

type tmResult struct {
	Samples []tmSample `json:"samples"`
	Meta    tmMeta     `json:"meta"`
}

func runTextMetrics(t *testing.T, page *rod.Page) tmResult {
	t.Helper()

	var res tmResult
	raw := page.Timeout(probeEvalTimeout).MustEval(textMetricsJS).Str()
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("decode text-metrics probe: %v\n%s", err, raw)
	}
	return res
}

// sample finds one row of the matrix.
func (r tmResult) sample(t *testing.T, font, text string) tmSample {
	t.Helper()

	for _, s := range r.Samples {
		if s.Font == font && s.Text == text {
			return s
		}
	}
	t.Fatalf("no sample for %q / %q", font, text)
	return tmSample{}
}

const (
	tmProbeText = "mmmmmmmmmmlli"
	tmLongText  = "The quick brown fox jumps over the lazy dog"
)

// TestProbeTextMetrics is the acceptance gate for issue #15.
//
// It measures the unshimmed page and the production page in one launch, prints
// both, and asserts on the production one only. The unshimmed column is not
// asserted: it is the bug, and the bug is the browser's, not ours.
func TestProbeTextMetrics(t *testing.T) {
	url, stop := probeServer(t)
	defer stop()

	b := launchProbeBrowser(t)
	defer b.Close()

	// A page straight off the rod browser skips headless_browser.NewPage and so
	// skips the page hook: this is exactly what the build does unrepaired.
	rawPage := b.Rod().MustPage()
	rawPage.MustNavigate(url).MustWaitLoad()
	before := runTextMetrics(t, rawPage)
	rawPage.MustClose()

	after := runTextMetrics(t, openProbePage(t, b, url))

	rows := []row{}
	add := func(sig, val, verdict string) { rows = append(rows, row{sig, val, verdict}) }

	for _, f := range []string{"72px monospace", "72px Arial", "16px monospace"} {
		bs := before.sample(t, f, tmProbeText)
		as := after.sample(t, f, tmProbeText)
		add(fmt.Sprintf("%s  unshimmed", f), fmt.Sprintf("%.6g px", bs.Width), vBroken)
		add(fmt.Sprintf("%s  repaired", f),
			fmt.Sprintf("%.4f px (DOM %.4f, error %+.4f)", as.Width, as.Dom, as.Width-as.Dom),
			verdictFor(math.Abs(as.Width-as.Dom) < 0.5))
	}
	add("measureText.toString()", after.Meta.MeasureTextToString,
		verdictFor(after.Meta.MeasureTextToString == "function measureText() { [native code] }"))
	add("TextMetrics width getter toString()", after.Meta.WidthGetterToString,
		verdictFor(!containsJSSource(after.Meta.WidthGetterToString)))
	add("Function.prototype.toString.toString()", after.Meta.ToStringToString,
		verdictFor(after.Meta.ToStringToString == "function toString() { [native code] }"))
	add("metrics instanceof TextMetrics", yesNo(after.Meta.InstanceOf), verdictFor(after.Meta.InstanceOf))
	add("Object.prototype.toString.call(metrics)", after.Meta.Brand, verdictFor(after.Meta.Brand == "[object TextMetrics]"))
	add("native getter called on the instance", fmt.Sprintf("%v", after.Meta.GetterOnInstance),
		verdictFor(!isErrString(after.Meta.GetterOnInstance)))
	add("same-origin iframe repaired", fmt.Sprintf("%.4f px", after.Meta.Iframe), verdictFor(after.Meta.Iframe > 1))
	add("OffscreenCanvas repaired", fmt.Sprintf("%.4f px", after.Meta.Offscreen), verdictFor(after.Meta.Offscreen > 1))
	add("measureText('') width", fmt.Sprintf("%g px", after.Meta.Empty), verdictFor(after.Meta.Empty == 0))

	printTable(fmt.Sprintf("CANVAS TEXT METRICS  (issue #15, seed=%d)", probeSeed), rows)

	// --- the gate ----------------------------------------------------------

	for _, s := range after.Samples {
		if s.Width <= 0 {
			t.Errorf("#15: measureText(%q) at %s returned %g, want a positive width", s.Text, s.Font, s.Width)
		}
		// Every field moved with the same factor, so a plausible width beside a
		// zeroed ascent would mean the repair reached one and not the others.
		if s.FbA <= 0 || s.FbD <= 0 || s.AbbA <= 0 {
			t.Errorf("#15: %s / %q has a zeroed box: fontAscent=%g fontDescent=%g actualAscent=%g",
				s.Font, s.Text, s.FbA, s.FbD, s.AbbA)
		}
		// The repaired width is the browser's own layout width, so it has to
		// agree with what the DOM reports for the same string and font. Half a
		// pixel covers the jitter plus the DOM's 1/64 px snapping.
		if math.Abs(s.Width-s.Dom) > 0.5 {
			t.Errorf("#15: %s / %q canvas width %.4f disagrees with DOM width %.4f",
				s.Font, s.Text, s.Width, s.Dom)
		}
	}

	// Fonts must be distinguishable: that is the signal the broken build
	// destroys, and a shim that returned one width per size would destroy it
	// again while looking healthy.
	mono := after.sample(t, "72px monospace", tmProbeText).Width
	arial := after.sample(t, "72px Arial", tmProbeText).Width
	times := after.sample(t, `72px "Times New Roman"`, tmProbeText).Width
	t.Logf("72px, %q: monospace %.4f | Arial %.4f | Times New Roman %.4f", tmProbeText, mono, arial, times)
	if math.Abs(mono-arial) < 1 || math.Abs(mono-times) < 1 || math.Abs(arial-times) < 1 {
		t.Errorf("#15: fonts are not distinguishable: mono=%.4f arial=%.4f times=%.4f", mono, arial, times)
	}

	// Length and size have to move the width the way they do in a real browser.
	short := after.sample(t, "72px monospace", "i").Width
	long := after.sample(t, "72px monospace", tmLongText).Width
	if !(short < mono && mono < long) {
		t.Errorf("#15: width does not grow with string length: 1ch=%.4f 13ch=%.4f 43ch=%.4f", short, mono, long)
	}
	small := after.sample(t, "16px monospace", tmProbeText).Width
	medium := after.sample(t, "32px monospace", tmProbeText).Width
	if ratio := medium / small; math.Abs(ratio-2) > 0.05 {
		t.Errorf("#15: width does not scale with font size: 16px=%.4f 32px=%.4f (ratio %.4f, want ~2)",
			small, medium, ratio)
	}

	// Determinism. Noise that re-rolls per call is a louder tell than the bug.
	for _, v := range after.Meta.Repeat {
		if v != after.Meta.Repeat[0] {
			t.Errorf("#15: measureText is not deterministic across calls: %v", after.Meta.Repeat)
			break
		}
	}

	if after.Meta.MeasureTextToString != "function measureText() { [native code] }" {
		t.Errorf("#15: measureText no longer stringifies as a native method: %q", after.Meta.MeasureTextToString)
	}
	if containsJSSource(after.Meta.WidthGetterToString) {
		t.Errorf("#15: the width getter stringifies as JS source: %q", after.Meta.WidthGetterToString)
	}
	if after.Meta.ToStringToString != "function toString() { [native code] }" {
		t.Errorf("#15: Function.prototype.toString itself was patched: %q", after.Meta.ToStringToString)
	}
	if !after.Meta.InstanceOf || after.Meta.Brand != "[object TextMetrics]" || after.Meta.OwnProps != 0 {
		t.Errorf("#15: the returned object is not a plain TextMetrics: instanceof=%v brand=%q ownProps=%d",
			after.Meta.InstanceOf, after.Meta.Brand, after.Meta.OwnProps)
	}
	if isErrString(after.Meta.GetterOnInstance) {
		t.Errorf("#15: the width getter rejects its own instance: %v", after.Meta.GetterOnInstance)
	}
	if !after.Meta.WidthEnumerable || !after.Meta.WidthConfigurable || after.Meta.WidthHasSetter {
		t.Errorf("#15: the width descriptor no longer matches a native accessor: enumerable=%v configurable=%v setter=%v",
			after.Meta.WidthEnumerable, after.Meta.WidthConfigurable, after.Meta.WidthHasSetter)
	}
	if after.Meta.Iframe <= 1 {
		t.Errorf("#15: the shim does not reach same-origin iframes: %g", after.Meta.Iframe)
	}
	if after.Meta.Offscreen <= 1 {
		t.Errorf("#15: the shim does not reach OffscreenCanvas: %g", after.Meta.Offscreen)
	}
}

// TestProbeTextMetricsPerSeed is the identity half of the repair, the same
// discipline as the geometry gate: the width one account reports must be the
// same width after a restart, and must not be the width another account
// reports. Restoring the true layout width alone would make every account on a
// machine measure identically, so the shim adds sub-pixel seed-derived noise on
// top — and that noise is what this test reads.
func TestProbeTextMetricsPerSeed(t *testing.T) {
	url, stop := probeServer(t)
	defer stop()

	measure := func(seed int) tmResult {
		bin, err := EnsureBrowser()
		if err != nil {
			t.Skipf("SKIP: bundled browser unavailable: %v", err)
		}
		cfg := newConfig(true, WithFingerprintSeed(seed))
		cfg.binPath = bin

		b := headless_browser.New(buildOptions(cfg)...)
		defer b.Close()
		return runTextMetrics(t, openProbePage(t, b, url))
	}

	otherSeed := probeSeed + 7919
	first := measure(probeSeed)
	second := measure(probeSeed)
	other := measure(otherSeed)

	w := func(r tmResult) float64 { return r.sample(t, "72px monospace", tmProbeText).Width }
	t.Logf("72px monospace %q: seed %d -> %.10f / relaunch %.10f ; seed %d -> %.10f",
		tmProbeText, probeSeed, w(first), w(second), otherSeed, w(other))

	if w(first) != w(second) {
		t.Errorf("#15: same seed gave different widths across launches: %.10f vs %.10f", w(first), w(second))
	}
	if w(first) == w(other) {
		t.Errorf("#15: two seeds share one width %.10f; the metrics carry no account identity", w(first))
	}
	// The difference must be noise, not a different browser: both are still the
	// machine's real layout width to within a fraction of a pixel.
	if d := math.Abs(w(first) - w(other)); d > 2*textMetricsJitterPx {
		t.Errorf("#15: per-seed spread %.4f px exceeds the %.4f px jitter budget; the widths are no longer plausible",
			d, 2*textMetricsJitterPx)
	}
}

func verdictFor(ok bool) string {
	if ok {
		return vClean
	}
	return vBroken
}

func containsJSSource(s string) bool {
	return !isNativeFnString(s)
}

func isNativeFnString(s string) bool {
	const marker = "[native code]"
	return len(s) > len(marker) && s[len(s)-len(marker)-2:len(s)-2] == marker
}

func isErrString(v any) bool {
	s, ok := v.(string)
	return ok && len(s) >= 3 && s[:3] == "ERR"
}
