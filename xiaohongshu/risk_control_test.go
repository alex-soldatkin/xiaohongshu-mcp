package xiaohongshu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func kindsOf(signals []riskSignal) map[string]int {
	out := map[string]int{}
	for _, s := range signals {
		out[s.kind]++
	}
	return out
}

// TestDetectURLSignals covers the strongest detector, and the case that keeps
// it from being the noisiest: "verify" in the query string is a search term,
// not a security interstitial.
func TestDetectURLSignals(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantHit bool
	}{
		{"website login captcha", "https://www.xiaohongshu.com/website-login/captcha?redirectPath=%2Fexplore", true},
		{"web login captcha", "https://www.xiaohongshu.com/web-login/captcha", true},
		{"verification interstitial", "https://www.xiaohongshu.com/verify/slider", true},
		{"explore", "https://www.xiaohongshu.com/explore", false},
		{"note detail", "https://www.xiaohongshu.com/explore/6539?xsec_token=ABC", false},
		{"search for the word verify", "https://www.xiaohongshu.com/search_result?keyword=verify+captcha", false},
		{"empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectURLSignals(tc.url)
			if tc.wantHit && len(got) == 0 {
				t.Fatalf("no signal for %s", tc.url)
			}
			if !tc.wantHit && len(got) != 0 {
				t.Fatalf("false positive for %s: %v", tc.url, got)
			}
			// One URL is one piece of evidence, never several.
			if len(got) > 1 {
				t.Fatalf("URL produced %d signals, want at most 1: %v", len(got), got)
			}
		})
	}
}

func TestDetectRiskSignals_SliderFixture(t *testing.T) {
	html := readFixture(t, "risk_slider_captcha.html")

	signals := detectRiskSignals("https://www.xiaohongshu.com/explore", html, true)
	if len(signals) == 0 {
		t.Fatal("slider fixture produced no signals")
	}
	if kindsOf(signals)[riskKindCaptchaDOM] == 0 {
		t.Fatalf("want a captcha_dom signal, got %v", signals)
	}

	// The container and the text should both be seen: that is what makes a real
	// challenge cross the two-signal threshold on its first check.
	var container, text bool
	for _, s := range signals {
		switch s.where {
		case "dom-container":
			container = true
		case "dom-text":
			text = true
		}
	}
	if !container || !text {
		t.Fatalf("want both a container and a text signal, got %v", signals)
	}
}

// TestDetectRiskSignals_LoginModal pins the condition: the login modal is only
// a risk signal when a cookie session was supposed to be in force.
func TestDetectRiskSignals_LoginModal(t *testing.T) {
	html := readFixture(t, "risk_login_modal.html")

	withSession := detectRiskSignals("https://www.xiaohongshu.com/explore", html, true)
	if kindsOf(withSession)[riskKindLoginRequire] == 0 {
		t.Fatalf("login modal with a cookie session must be reported, got %v", withSession)
	}

	withoutSession := detectRiskSignals("https://www.xiaohongshu.com/explore", html, false)
	if len(withoutSession) != 0 {
		t.Fatalf("login modal without a session is just logged out, got %v", withoutSession)
	}
}

// TestDetectRiskSignals_CleanNoteIsNotAChallenge is the test that matters most.
// The detectors exist to tell the operator the truth about account health; a
// detector that fires on a note about captchas tells a very confident lie.
func TestDetectRiskSignals_CleanNoteIsNotAChallenge(t *testing.T) {
	html := readFixture(t, "risk_clean_note.html")

	signals := detectRiskSignals("https://www.xiaohongshu.com/explore/653f?xsec_token=AB", html, true)
	if len(signals) != 0 {
		t.Fatalf("clean note reported as risk control: %v", signals)
	}
}

func TestDetectRiskSignals_EmptyHTML(t *testing.T) {
	if got := detectRiskSignals("https://www.xiaohongshu.com/explore", "", true); len(got) != 0 {
		t.Fatalf("empty page produced signals: %v", got)
	}
}

// newTestDetector builds a detector that touches neither the clock, the disk
// nor a browser.
func newTestDetector(t *testing.T) (*riskDetector, *int) {
	t.Helper()

	cooldowns := 0
	d := &riskDetector{
		window:          riskSignalWindow,
		threshold:       riskSignalThreshold,
		now:             time.Now,
		cooldown:        func(time.Duration) { cooldowns++ },
		screenshot:      func(*rod.Page, string) string { return "" },
		sessionExpected: func() bool { return true },
		headful:         func() bool { return false },
	}
	return d, &cooldowns
}

// TestRiskDetector_OneSignalReportsTwoSignalsCool pins the false-positive
// guard: the marker table is a set of guesses, so a lone hit is reported but
// does not cost a healthy account 30 minutes of silence.
func TestRiskDetector_OneSignalReportsTwoSignalsCool(t *testing.T) {
	d, cooldowns := newTestDetector(t)

	first := d.report(nil, "https://www.xiaohongshu.com/explore", []riskSignal{
		{kind: riskKindCaptchaDOM, marker: "安全验证", where: "dom-text"},
	})

	rc, ok := myerrors.AsRiskControl(first)
	if !ok {
		t.Fatalf("want *ErrRiskControl, got %T: %v", first, first)
	}
	if *cooldowns != 0 {
		t.Fatalf("one signal entered a cooldown (%d)", *cooldowns)
	}
	if rc.Cooldown != 0 {
		t.Fatalf("Cooldown = %s, want 0 on the first signal", rc.Cooldown)
	}

	second := d.report(nil, "https://www.xiaohongshu.com/explore", []riskSignal{
		{kind: riskKindCaptchaDOM, marker: "拖动滑块", where: "dom-text"},
	})

	rc2, _ := myerrors.AsRiskControl(second)
	if *cooldowns != 1 {
		t.Fatalf("cooldowns = %d, want exactly 1 after the second signal", *cooldowns)
	}
	if rc2.Cooldown <= 0 {
		t.Fatalf("Cooldown = %s, want the configured cooldown reported to the caller", rc2.Cooldown)
	}
}

// A single check that trips two independent detectors is corroborated on the
// spot: the rule is about corroboration, not about elapsed time.
func TestRiskDetector_TwoSignalsInOneCheckCool(t *testing.T) {
	d, cooldowns := newTestDetector(t)

	err := d.report(nil, "https://www.xiaohongshu.com/website-login/captcha", []riskSignal{
		{kind: riskKindCaptchaURL, marker: "/website-login/captcha", where: "url"},
		{kind: riskKindCaptchaDOM, marker: "captcha", where: "dom-container"},
	})

	if _, ok := myerrors.AsRiskControl(err); !ok {
		t.Fatalf("want *ErrRiskControl, got %v", err)
	}
	if *cooldowns != 1 {
		t.Fatalf("cooldowns = %d, want 1", *cooldowns)
	}
}

// Signals older than the window are not evidence of anything current.
func TestRiskDetector_StaleSignalsExpire(t *testing.T) {
	d, cooldowns := newTestDetector(t)

	now := time.Now()
	d.now = func() time.Time { return now }
	d.report(nil, "u", []riskSignal{{kind: riskKindCaptchaDOM, marker: "安全验证", where: "dom-text"}})

	now = now.Add(riskSignalWindow + time.Minute)
	d.report(nil, "u", []riskSignal{{kind: riskKindCaptchaDOM, marker: "安全验证", where: "dom-text"}})

	if *cooldowns != 0 {
		t.Fatalf("two signals %s apart tripped a cooldown", riskSignalWindow+time.Minute)
	}
}

// The headful message is the whole out-of-scope policy in one sentence: a human
// solves the challenge, we do not.
func TestRiskDetector_HeadfulDetailPointsAtTheWindow(t *testing.T) {
	d, _ := newTestDetector(t)
	d.headful = func() bool { return true }

	err := d.report(nil, "u", []riskSignal{{kind: riskKindCaptchaDOM, marker: "安全验证", where: "dom-text"}})
	if got := err.Error(); !strings.Contains(got, "solve the verification there") {
		t.Fatalf("headful error text = %q", got)
	}

	d2, _ := newTestDetector(t)
	err2 := d2.report(nil, "u", []riskSignal{{kind: riskKindCaptchaDOM, marker: "安全验证", where: "dom-text"}})
	if got := err2.Error(); !strings.Contains(got, "backing off") {
		t.Fatalf("headless error text = %q", got)
	}
}

// TestCollect_FakeRiskControl proves the debug switch reaches the plumbing,
// which is the only way to exercise any of this before a real challenge is seen.
func TestCollect_FakeRiskControl(t *testing.T) {
	t.Setenv("XHS_FAKE_RISK_CONTROL", "1")

	d, cooldowns := newTestDetector(t)

	_, signals := d.collect(nil)
	if len(signals) != 1 || signals[0].kind != riskKindFake {
		t.Fatalf("fake switch produced %v", signals)
	}

	if err := d.check(nil); err == nil {
		t.Fatal("fake switch must produce an error")
	}
	if *cooldowns != 0 {
		t.Fatalf("first fake signal entered a cooldown")
	}
	if err := d.check(nil); err == nil || *cooldowns != 1 {
		t.Fatalf("second fake signal must trip the cooldown (err=%v, cooldowns=%d)", err, *cooldowns)
	}
}
