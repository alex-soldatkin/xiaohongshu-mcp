package browser

import (
	"math"
	"strings"
	"testing"
)

// The live behaviour is gated by TestProbeTextMetrics under -tags integration.
// These are the hermetic halves: the decision about whether to shim at all, and
// the shape of the script that gets injected.

func TestTextMetricsCorrection(t *testing.T) {
	cases := []struct {
		name string
		k    float64
		want float64
	}{
		// The two factors measured on the bundled build, for seeds 98759 and
		// 106678. The second is positive: the sign is seed-derived too.
		{"measured negative factor", -1.8045853526281e-07, 1 / -1.8045853526281e-07},
		{"measured positive factor", 1.7902724365110048e-06, 1 / 1.7902724365110048e-06},
		// A build that measures text correctly needs no shim, and a shim on a
		// working API is a pure liability.
		{"healthy build", 1, 0},
		{"healthy within the band", 1.01, 0},
		// Anything between "obviously annihilated" and "obviously fine" is a
		// distortion nobody has characterised; correcting it would be a guess.
		{"uncharacterised distortion", 0.5, 0},
		{"just outside the broken band", 0.2, 0},
		{"nonsense", math.NaN(), 0},
		{"zero", 0, 0},
		{"infinite", math.Inf(-1), 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := textMetricsCorrection(c.k); got != c.want {
				t.Errorf("textMetricsCorrection(%g) = %g, want %g", c.k, got, c.want)
			}
		})
	}
}

// The jitter salt is the account's identity in this shim: same seed, same
// sub-pixel noise; different seed, different noise.
func TestTextMetricsSaltPerSeed(t *testing.T) {
	a := newTextMetricsInstaller(98759)
	b := newTextMetricsInstaller(98759)
	c := newTextMetricsInstaller(106678)

	if a.salt != b.salt {
		t.Errorf("same seed gave two salts: %d vs %d", a.salt, b.salt)
	}
	if a.salt == c.salt {
		t.Errorf("two seeds share one salt: %d", a.salt)
	}
}

func TestTextMetricsScript(t *testing.T) {
	js := textMetricsScript(-5.541439192907414e+06, 12345)

	for _, want := range []string{
		"-5.541439192907414e+06", // the correction, baked in
		"var SALT = 12345",       // the account's jitter salt
		"TextMetrics",
		"new Proxy",   // native-looking stringification for the accessors
		"Reflect.app", // the native getter keeps its real receiver
	} {
		if !strings.Contains(js, want) {
			t.Errorf("injected script does not contain %q:\n%s", want, js)
		}
	}

	// The shim must stay narrow: measureText itself is left native, and
	// Function.prototype.toString is not patched. Both are detection surfaces
	// of their own, and both were deliberately kept out of it.
	for _, unwanted := range []string{
		"CanvasRenderingContext2D",
		"Function.prototype.toString",
	} {
		if strings.Contains(js, unwanted) {
			t.Errorf("injected script touches %q; the shim is meant to be TextMetrics accessors only:\n%s", unwanted, js)
		}
	}
}
