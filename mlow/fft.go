package mlow

import (
	"math"
	"sync"
)

// cpx is a single-precision complex value.
//
// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/674e85164b35ca19115dfebcf605708d15951ee7/wacore/src/voip/mlow/smpl_perc.rs#L318-L343
type cpx struct {
	re, im float32
}

func (a cpx) add(b cpx) cpx {
	return cpx{re: a.re + b.re, im: a.im + b.im}
}

func (a cpx) mul(b cpx) cpx {
	return cpx{
		re: a.re*b.re - a.im*b.im,
		im: a.re*b.im + a.im*b.re,
	}
}

// smallestFactor returns the smallest prime factor of n (>= 2).
func smallestFactor(n int) int {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/674e85164b35ca19115dfebcf605708d15951ee7/wacore/src/voip/mlow/smpl_perc.rs#L346-L358
	if n%2 == 0 {
		return 2
	}
	p := 3
	for p*p <= n {
		if n%p == 0 {
			return p
		}
		p += 2
	}
	return n
}

// fftLevel is one recursion level of the mixed-radix transform: a block of n inputs is
// split into p interleaved sub-transforms of m = n/p points. For a prime n (p == n) the
// level is the direct DFT. tw holds the level's twiddle factors, tw[k*p+q] (or tw[k*n+j]
// for the direct DFT), computed once with the reference expression so every product is
// bit-identical to evaluating cos/sin inline.
type fftLevel struct {
	n, p, m int
	tw      []cpx
}

// fftPlan is the precomputed transform for one (length, direction) pair. The recursion
// shape depends only on the length, so the twiddles and the per-level work buffers are
// built once and reused; scratch buffers are pooled so concurrent encoders never share one.
type fftPlan struct {
	n      int
	levels []fftLevel
	pool   sync.Pool
}

// fftScratch is the reusable working memory of one transform: one sub-result buffer per
// composite level plus the complex input/output of the real-FFT wrappers.
type fftScratch struct {
	subs     [][]cpx
	cin, out []cpx
}

var fftPlans sync.Map // fftPlanKey → *fftPlan

type fftPlanKey struct {
	n       int
	inverse bool
}

// fftPlanFor returns the cached plan for an n-point transform. sign is -1 forward, +1
// inverse (unnormalized).
func fftPlanFor(n int, sign float32) *fftPlan {
	key := fftPlanKey{n: n, inverse: sign > 0}
	if p, ok := fftPlans.Load(key); ok {
		return p.(*fftPlan)
	}
	plan := &fftPlan{n: n}
	for size := n; size > 1; {
		p := smallestFactor(size)
		lv := fftLevel{n: size, p: p, m: size / p}
		if p == size {
			lv.tw = make([]cpx, size*size)
			for k := 0; k < size; k++ {
				angK := sign * 2.0 * smplPI * float32(k) / float32(size)
				for j := 0; j < size; j++ {
					ang := angK * float32(j)
					lv.tw[k*size+j] = cpx{re: float32(math.Cos(float64(ang))), im: float32(math.Sin(float64(ang)))}
				}
			}
		} else {
			lv.tw = make([]cpx, size*p)
			for k := 0; k < size; k++ {
				for q := 0; q < p; q++ {
					ang := sign * 2.0 * smplPI * float32(k) * float32(q) / float32(size)
					lv.tw[k*p+q] = cpx{re: float32(math.Cos(float64(ang))), im: float32(math.Sin(float64(ang)))}
				}
			}
		}
		plan.levels = append(plan.levels, lv)
		if p == size {
			break
		}
		size /= p
	}
	plan.pool.New = func() any {
		sc := &fftScratch{cin: make([]cpx, n), out: make([]cpx, n)}
		for _, lv := range plan.levels {
			if lv.p != lv.n {
				sc.subs = append(sc.subs, make([]cpx, lv.n))
			}
		}
		return sc
	}
	actual, _ := fftPlans.LoadOrStore(key, plan)
	return actual.(*fftPlan)
}

// rec is the recursive mixed-radix Cooley-Tukey DFT at recursion depth level. x holds
// the level's inputs at the given stride; out is contiguous. The operation order matches
// the reference recursion exactly; only the trig evaluation and the allocations moved
// into the plan.
func (pl *fftPlan) rec(level int, x []cpx, stride int, out []cpx, sc *fftScratch) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/674e85164b35ca19115dfebcf605708d15951ee7/wacore/src/voip/mlow/smpl_perc.rs#L362-L405
	if level >= len(pl.levels) {
		out[0] = x[0]
		return
	}
	lv := &pl.levels[level]
	n, p, m := lv.n, lv.p, lv.m
	if p == n {
		for k := 0; k < n; k++ {
			var acc cpx
			tw := lv.tw[k*n : k*n+n]
			for j := 0; j < n; j++ {
				acc = acc.add(x[j*stride].mul(tw[j]))
			}
			out[k] = acc
		}
		return
	}
	sub := sc.subs[level]
	for q := 0; q < p; q++ {
		pl.rec(level+1, x[q*stride:], stride*p, sub[q*m:(q+1)*m], sc)
	}
	for k := 0; k < n; k++ {
		kmod := k % m
		var acc cpx
		tw := lv.tw[k*p : k*p+p]
		for q := 0; q < p; q++ {
			acc = acc.add(sub[q*m+kmod].mul(tw[q]))
		}
		out[k] = acc
	}
}

// cfft computes the complex FFT of a mixed-radix length into out. sign=-1 forward,
// +1 inverse.
func cfft(input, out []cpx, sign float32) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/674e85164b35ca19115dfebcf605708d15951ee7/wacore/src/voip/mlow/smpl_perc.rs#L408-L412
	pl := fftPlanFor(len(input), sign)
	sc := pl.pool.Get().(*fftScratch)
	pl.rec(0, input, 1, out, sc)
	pl.pool.Put(sc)
}

// rfftForwardOrdered is the forward real FFT of n real samples, re-packed into the
// ordered REAL layout: f[0]=DC.re, f[1]=Nyquist.re, then [re,im] pairs for bins
// 1..n/2-1. Output length is n.
func rfftForwardOrdered(time, f []float32) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/674e85164b35ca19115dfebcf605708d15951ee7/wacore/src/voip/mlow/smpl_perc.rs#L416-L432
	n := len(time)
	pl := fftPlanFor(n, -1.0)
	sc := pl.pool.Get().(*fftScratch)
	defer pl.pool.Put(sc)
	cin, spec := sc.cin, sc.out
	for i := 0; i < n; i++ {
		cin[i] = cpx{re: time[i]}
	}
	pl.rec(0, cin, 1, spec, sc)
	f[0] = spec[0].re
	f[1] = spec[n/2].re
	for i := 1; i < n/2; i++ {
		f[2*i] = spec[i].re
		f[2*i+1] = spec[i].im
	}
}
