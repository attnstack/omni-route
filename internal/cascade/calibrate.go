// calibrate.go：级联升级阈值的在线校准器。
// 目标升级率 ≈ 30%（配置）：升级率过高说明阈值太严（小模型白跑一次），
// 过低说明该升没升（质量风险）。控制律：θ ← θ − step·(rate − target)，
// 注意符号——升级率超目标时 θ 要往下调（放宽放行线），升级才会变少。
//
// 阈值是全局单一的：校准需要统计样本，分任务阈值在小流量任务上永远凑不满
// 一个窗口（实测教训：24 样本 ÷ 6 类任务 = 每桶 4 条，分桶校准形同虚设，
// 七轮真实基准里闭环从未转动）。全局升级率驱动全局 θ；
// 分任务阈值需要每任务流量保障，属后续工作。
package cascade

import (
	"sync"
)

// Calibrator 维护全局阈值 θ 与一个滚动升级率窗口。
// Threshold(task) 保留 task 参数以兼容调用方语义，当前实现忽略它。
type Calibrator struct {
	mu       sync.Mutex
	theta    float64
	window   []int
	idx      int
	filled   int
	target   float64
	step     float64
	min, max float64
	base     float64
}

func NewCalibrator(baseThreshold, target, step float64, window int) *Calibrator {
	if window <= 0 {
		window = 50
	}
	if step <= 0 {
		step = 0.05
	}
	// 阈值活动范围随基线自适应，且硬夹在 [0.05, 0.98]——校准失控不能把
	// 判别器变成"永远升级"或"永远放行"。
	lo := baseThreshold - 0.2
	if lo < 0.05 {
		lo = 0.05
	}
	hi := baseThreshold + 0.3
	if hi > 0.98 {
		hi = 0.98
	}
	return &Calibrator{
		theta:  baseThreshold,
		window: make([]int, window),
		target: target,
		step:   step,
		min:    lo,
		max:    hi,
		base:   baseThreshold,
	}
}

func clampF(v, lo, hi float64) float64 {
	if lo < 0.05 {
		lo = 0.05
	}
	if hi > 0.98 {
		hi = 0.98
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Threshold 返回当前全局阈值。
func (c *Calibrator) Threshold(task string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.theta
}

// Observe 记录一次级联决策是否升级；每满一个窗口做一次阈值调整。
func (c *Calibrator) Observe(task string, upgraded bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := 0
	if upgraded {
		v = 1
	}
	c.window[c.idx] = v
	c.idx = (c.idx + 1) % len(c.window)
	if c.filled < len(c.window) {
		c.filled++
	}
	if c.filled < len(c.window) {
		return // 样本不满不做调整——小样本调阈值是自我扰动
	}
	sum := 0
	for _, x := range c.window {
		sum += x
	}
	rate := float64(sum) / float64(len(c.window))
	c.theta = clampF(c.theta-c.step*(rate-c.target), c.min, c.max)
}

// Snapshot 供观测面板（全局阈值以 "_global" 键输出）。
func (c *Calibrator) Snapshot() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]float64{"_global": c.theta}
}
