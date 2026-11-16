package web

import (
	"testing"
	"time"
)

// The comparison shows every step difference from 0.1 s, its share of
// the earlier time, and marks the large ones (at least 1 s and 20 %, or
// 30 s); the feed's noise filter (delta) is not used here.
func TestStepDelta(t *testing.T) {
	const ms, s = time.Millisecond, time.Second
	for _, c := range []struct {
		later, earlier time.Duration
		text, pct      string
		slower, big    bool
	}{
		{7*time.Minute + 49*s, 6*time.Minute + 40*s, "+1\u00a0min 9\u00a0s", "17\u00a0%", true, true}, // >= 30 s, 17 %
		{3700 * ms, 5 * s, "−1.3\u00a0s", "26\u00a0%", false, true},
		{1900 * ms, 1 * s, "+0.9\u00a0s", "90\u00a0%", true, false}, // below 1 s
		{44 * s, 42 * s, "+2.0\u00a0s", "4\u00a0%", true, false},    // 1 s, but below 20 %
		{9900 * ms, 9400 * ms, "+0.5\u00a0s", "5\u00a0%", true, false},
		{3400 * ms, 3100 * ms, "+0.3\u00a0s", "9\u00a0%", true, false}, // 9.67 % cut down
		{10 * time.Minute, 9 * time.Minute, "+1\u00a0min 0\u00a0s", "11\u00a0%", true, true},
		{12 * s, 0, "+12\u00a0s", "", true, true}, // nothing to take a share of
		{0, 0, "", "", false, false},
		{40 * ms, 0, "", "", false, false},                         // shown as 0.0 s on both sides
		{5 * s, 5040 * ms, "", "", false, false},                   // 5.0 s both
		{6 * s, 5 * s, "+1.0\u00a0s", "20\u00a0%", true, true},     // exactly at the mark
		{5990 * ms, 5 * s, "+1.0\u00a0s", "20\u00a0%", true, true}, // as shown: 6.0 s vs 5.0 s
		{400*s + 300*ms, 400 * s, "", "", false, false},            // 6 min 40 s both
		{401 * s, 400 * s, "+1.0\u00a0s", "<1\u00a0%", true, false},
		{-s, 2 * s, "−2.0\u00a0s", "100\u00a0%", false, true}, // a clock stepped back: none
	} {
		text, pct, slower, big := stepDelta(c.later, c.earlier)
		if text != c.text || pct != c.pct || slower != c.slower || big != c.big {
			t.Errorf("stepDelta(%v, %v) = %q %q slower=%v big=%v, want %q %q slower=%v big=%v",
				c.later, c.earlier, text, pct, slower, big, c.text, c.pct, c.slower, c.big)
		}
	}
	// The feed keeps its filter.
	if d, _ := delta(7*time.Minute+49*s, 6*time.Minute+40*s); d != "" {
		t.Errorf("feed delta changed: %q", d)
	}
}

// Steps keep the later build's order; every paired step gets its
// difference, small ones too.
func TestCompareStepsShowsSmallDifferences(t *testing.T) {
	a := []StepView{{Key: "[build 5/8]", Name: "RUN make Image", State: "built", D: 400 * time.Second},
		{Key: "[build 6/8]", Name: "RUN make modules_install", State: "built", D: time.Second},
		{Key: "[build 2/8]", Name: "RUN useradd build", State: "cached"}}
	b := []StepView{{Key: "[build 2/8]", Name: "RUN useradd build", State: "cached"},
		{Key: "[build 5/8]", Name: "RUN make Image", State: "built", D: 469 * time.Second},
		{Key: "[build 6/8]", Name: "RUN make modules_install", State: "built", D: 1900 * time.Millisecond}}
	got := compareSteps(a, b)
	if len(got) != 3 || got[0].Key != "[build 2/8]" || got[1].Key != "[build 5/8]" || got[2].Key != "[build 6/8]" {
		t.Fatalf("order: %+v", got)
	}
	if got[0].Delta != "" {
		t.Errorf("no difference shown as %q", got[0].Delta)
	}
	if g := got[1]; g.Delta != "+1\u00a0min 9\u00a0s" || g.Pct != "17\u00a0%" || !g.Slower || !g.Big {
		t.Errorf("make Image: %+v", g)
	}
	if g := got[2]; g.Delta != "+0.9\u00a0s" || g.Pct != "90\u00a0%" || !g.Slower || g.Big {
		t.Errorf("modules_install: %+v", g)
	}
}
