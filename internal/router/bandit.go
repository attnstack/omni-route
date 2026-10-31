// bandit.go：Beta 后验采样（Thompson Sampling 的核心随机源）。
// Beta(a,b) = Gamma(a)·x / (Gamma(a)+Gamma(b))，Gamma 用 Marsaglia-Tsang 拒绝采样。
// 为什么不用查表/近似？——采样质量决定探索-利用平衡，精确采样
// 只有 30 行且零依赖；而 shape<1 时用 Stuart 定理缩样技巧保证任意参数可用。
package router

import "math"

// sampleBeta 从 Beta(a,b) 分布采样一个值。
// rng 注入随机源（func() float64），保证 benchmark 与测试可复现。
func sampleBeta(a, b float64, rng func() float64) float64 {
	if a <= 0 && b <= 0 {
		return 0.5
	}
	if a <= 0 {
		return 0
	}
	if b <= 0 {
		return 1
	}
	x := randGamma(a, rng)
	y := randGamma(b, rng)
	if x+y <= 0 {
		return 0.5
	}
	return x / (x + y)
}

// randGamma 用 Marsaglia-Tsang 方法采样 Gamma(shape, 1)。
// shape<1 时借助 Gamma(k) = Gamma(k+1)·U^(1/k)（k=shape）缩样。
func randGamma(shape float64, rng func() float64) float64 {
	if shape < 1 {
		return randGamma(shape+1, rng) * math.Pow(rng(), 1.0/shape)
	}
	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for {
		x := randNormal(rng)
		v := math.Pow(1+c*x, 3)
		if v <= 0 {
			continue
		}
		u := rng()
		// 快速接受路径：省一次 log 计算（该采样在每次请求都会跑）
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

func randNormal(rng func() float64) float64 {
	// Box-Muller；+1e-12 防 log(0)
	return math.Sqrt(-2*math.Log(rng()+1e-12)) * math.Cos(2*math.Pi*rng())
}
