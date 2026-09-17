package browser

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
)

// Canvas text metrics repair (issue #15).
//
// On the bundled build every TextMetrics field comes back as a signed near-zero
// float: measureText('mmmmmmmmmmlli').width at 72px monospace returns
// -0.000102 where the same machine's DOM layout gives 563.53. The fingerprint
// noise is applied multiplicatively where it should be additive, so it does not
// perturb the measurement, it annihilates it.
//
// Measured properties of the damage, which are what make a repair possible:
//
//   - It is one scale factor, not per-field noise. 72px monospace and 72px
//     Arial give k = -1.8045853526281e-7 and -1.804590232643636e-7 against
//     their DOM widths; ratios between strings are exact (13 chars / 1 char =
//     13.0000, 32px / 16px = 2.0000).
//   - The factor is derived from the fingerprint seed: seed 98759 gives
//     -1.8045853526281e-7, seed 106678 gives +1.7902724365110048e-6 (note the
//     sign flip), and an unpinned seed re-rolls it every launch.
//   - It is identical in every document of a launch, including same-origin
//     iframes and OffscreenCanvas, and identical across relaunches of one seed.
//
// So the true metric is still in the number; only its scale is gone. The repair
// calibrates k once per launch on about:blank — canvas against DOM layout for a
// known string, in a document the site never sees — and injects a shim that
// multiplies every TextMetrics field by 1/k before returning it.
//
// This is a deliberate, single exception to WithStealthJS(false). It is narrow
// on purpose: measureText itself is left genuinely native, so the most commonly
// stringified surface (CanvasRenderingContext2D.prototype.measureText.toString())
// is untouched, and only the accessors on TextMetrics.prototype are replaced.
// Each replacement is a Proxy over the native getter, which stringifies as
// "[native code]" rather than as JS source, and which forwards the real
// receiver so the native getter is invoked normally — wrapping the returned
// TextMetrics object in a Proxy instead was measured and fails, because the
// native getter rejects a proxied receiver with "Illegal invocation".

// textMetricsProbe is the calibration measurement: a string wide enough that
// the DOM width is unambiguous, in a generic family that resolves everywhere.
const (
	textMetricsProbeText = "mmmmmmmmmmlli"
	textMetricsProbeFont = "72px monospace"
)

// textMetricsJitterPx is the half-range of the sub-pixel noise added to the
// horizontal, text-dependent metrics after rescaling. It is what the patched
// build should have applied in the first place: additive, a fraction of a
// pixel, derived from the account's seed so it is stable per account and
// differs between accounts, and far too small to disturb layout code that
// measures text for truncation or label fitting.
const textMetricsJitterPx = 0.02

// textMetricsHealthyBand is how close to 1 the measured factor has to be for
// the build to count as unbroken. A fixed build needs no shim, and installing
// one anyway would be a JS override bought for nothing.
const textMetricsHealthyBand = 0.02

// textMetricsMaxFactor is the upper bound on a factor we are willing to call
// "broken the way #15 describes". The measured factors are ~1e-7 to ~1e-6; a
// value between that and 1 is some distortion we have not characterised, and
// guessing at a correction for it is worse than leaving it alone.
const textMetricsMaxFactor = 0.1

// calibrationTimeout bounds the one eval the repair costs per launch.
const calibrationTimeout = 10 * time.Second

// calibrateJS measures the canvas-versus-DOM ratio for one known string. It
// runs on about:blank inside the page hook, before any navigation, so the DOM
// node it appends and removes is never visible to the site.
var calibrateJS = fmt.Sprintf(`() => {
  const out = {};
  try {
    const ctx = document.createElement('canvas').getContext('2d');
    ctx.font = %[2]q;
    const span = document.createElement('span');
    span.textContent = %[1]q;
    span.style.cssText = 'position:absolute;left:-9999px;top:-9999px;white-space:nowrap;font:' + %[2]q;
    (document.body || document.documentElement).appendChild(span);
    out.dom = span.getBoundingClientRect().width;
    span.remove();
    out.canvas = ctx.measureText(%[1]q).width;
  } catch (e) {
    out.err = String(e && e.message || e);
  }
  return JSON.stringify(out);
}`, textMetricsProbeText, textMetricsProbeFont)

type calibration struct {
	Dom    float64 `json:"dom"`
	Canvas float64 `json:"canvas"`
	Err    string  `json:"err"`
}

// measureTextFactor returns the multiplicative damage factor k, such that
// reported == k * true. It is measured rather than assumed because k is
// seed-derived and this code does not know the patch's formula.
func measureTextFactor(page *rod.Page) (float64, error) {
	// Bounded: this runs inside the page hook, on the path every action takes
	// to get a page, and a hung eval there would hang the whole tool. A missed
	// calibration only costs the repair.
	raw, err := page.Timeout(calibrationTimeout).Eval(calibrateJS)
	if err != nil {
		return 0, fmt.Errorf("calibration eval: %w", err)
	}

	var c calibration
	if err := json.Unmarshal([]byte(raw.Value.Str()), &c); err != nil {
		return 0, fmt.Errorf("calibration decode: %w", err)
	}
	if c.Err != "" {
		return 0, fmt.Errorf("calibration failed in page: %s", c.Err)
	}
	// The DOM reference has to be a real layout. About:blank lays out normally
	// (measured: 563.53, the same width the loaded page reports), but a future
	// change that calibrates somewhere without layout would otherwise divide by
	// a zero that looks like a perfectly good float.
	if c.Dom <= 1 {
		return 0, fmt.Errorf("DOM reference width %v is not a layout", c.Dom)
	}
	if c.Canvas == 0 || math.IsNaN(c.Canvas) || math.IsInf(c.Canvas, 0) {
		return 0, fmt.Errorf("canvas width %v carries no signal", c.Canvas)
	}
	return c.Canvas / c.Dom, nil
}

// textMetricsScript renders the shim with the correction and the account's
// jitter salt baked in, so the page holds no code that could recompute either.
func textMetricsScript(scale float64, salt uint32) string {
	return fmt.Sprintf(`(function () {
  var TM = window.TextMetrics;
  if (!TM || !TM.prototype) return;
  var SCALE = %v;
  var SALT = %d;
  var JITTER = %v;
  // The horizontal, text-dependent metrics: these are the font-enumeration
  // surface, and the only ones where per-account noise means anything. The
  // vertical metrics are per-font constants that two real machines with the
  // same font already share, so they are rescaled and left alone.
  var jittered = {width: true, actualBoundingBoxLeft: true, actualBoundingBoxRight: true};

  // FNV-1a over the rescaled value, so the same measurement always yields the
  // same offset. Real metrics are deterministic; noise that re-rolled per call
  // would be a louder tell than the one being fixed.
  function offset(v) {
    var s = v.toFixed(6);
    var h = SALT >>> 0;
    for (var i = 0; i < s.length; i++) {
      h = Math.imul(h ^ s.charCodeAt(i), 16777619) >>> 0;
    }
    return ((h / 4294967296) * 2 - 1) * JITTER;
  }

  var names = Object.getOwnPropertyNames(TM.prototype);
  for (var i = 0; i < names.length; i++) {
    var name = names[i];
    if (name === 'constructor') continue;
    var d = Object.getOwnPropertyDescriptor(TM.prototype, name);
    if (!d || typeof d.get !== 'function') continue;

    Object.defineProperty(TM.prototype, name, {
      get: (function (nativeGet, jit) {
        return new Proxy(nativeGet, {
          apply: function (target, self, args) {
            var v = Reflect.apply(target, self, args);
            if (typeof v !== 'number' || !isFinite(v) || v === 0) return v;
            v = v * SCALE;
            return jit ? v + offset(v) : v;
          }
        });
      })(d.get, jittered[name] === true),
      set: d.set,
      enumerable: d.enumerable,
      configurable: d.configurable
    });
  }
})();`, scale, salt, textMetricsJitterPx)
}

// textMetricsInstaller calibrates once per browser launch and installs the shim
// on every page of it.
//
// The factor is a property of the launch, not of the page, so measuring it per
// page would cost an eval per action for a value that cannot have changed. A
// failed calibration is not cached: the next page tries again, and until one
// succeeds no shim is installed at all.
type textMetricsInstaller struct {
	salt uint32

	mu     sync.Mutex
	scale  float64
	done   bool // calibrated; scale==0 means "healthy or uncharacterised, do not shim"
	logged bool
}

func newTextMetricsInstaller(seed int) *textMetricsInstaller {
	return &textMetricsInstaller{salt: uint32(seedHash(seed, "textmetrics"))}
}

// install puts the shim on one page. It is called from the page hook, on a
// fresh about:blank page, so the script is registered before the first
// navigation and applies to every document the page then loads.
func (t *textMetricsInstaller) install(page *rod.Page) error {
	scale, err := t.resolveScale(page)
	if err != nil {
		return err
	}
	if scale == 0 {
		return nil
	}

	_, err = proto.PageAddScriptToEvaluateOnNewDocument{
		Source: textMetricsScript(scale, t.salt),
	}.Call(page)
	return err
}

// resolveScale returns the correction to apply, or 0 for "do not shim".
func (t *textMetricsInstaller) resolveScale(page *rod.Page) (float64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.done {
		return t.scale, nil
	}

	k, err := measureTextFactor(page)
	if err != nil {
		return 0, err
	}
	t.done = true
	t.scale = textMetricsCorrection(k)

	switch {
	case t.scale != 0:
		logrus.Infof("canvas text metrics repaired (#15): build scales them by %g, correcting by %g", k, t.scale)
	case math.Abs(k-1) <= textMetricsHealthyBand:
		// A build that reports real widths. Nothing to repair, and a shim on a
		// working API is a pure liability.
		logrus.Debugf("canvas text metrics look healthy (factor %g); no shim installed", k)
	default:
		logrus.Warnf("canvas text metrics distorted by an uncharacterised factor %g; leaving them alone (#15)", k)
	}
	return t.scale, nil
}

// textMetricsCorrection turns a measured damage factor into the correction to
// apply, or 0 for "install nothing". Separated from the measurement so the
// decision is testable without a browser.
func textMetricsCorrection(k float64) float64 {
	if math.IsNaN(k) || math.IsInf(k, 0) || k == 0 {
		return 0
	}
	if math.Abs(k-1) <= textMetricsHealthyBand {
		return 0
	}
	if math.Abs(k) > textMetricsMaxFactor {
		return 0
	}
	return 1 / k
}
