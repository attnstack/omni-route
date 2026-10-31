// router 包单测：状态更新（EMA/λ/消费窗口）、Thompson 收敛性、决策表。
package router

import (
	"fmt"
	"math"
	"testing"

	"github.com/attnstack/omni-route/internal/classifier"
	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

func testConfig() *config.Config {
	return &config.Config{
		Seed: 42,
		Routing: config.Routing{
			Mode:         "lagrangian",
			DefaultModel: "m-b",
			BudgetPerTask: map[string]float64{
				"code": 1.0, "chat": 1.0, "reasoning": 1.0,
				"summarization": 1.0, "translation": 1.0, "extraction": 1.0,
			},
			Lambda: config.LambdaCfg{Init: 100, Min: 1, Max: 5000, Eta: 0.1},
		},
	}
}

func TestSampleBeta(t *testing.T) {
	rng := deterministicRand(7)
	// Beta(100, 2) 的均值 ≈ 0.98：10000 次采样均值必须明显偏 1
	sum := 0.0
	n := 10000
	for i := 0; i < n; i++ {
		sum += sampleBeta(100, 2, rng)
	}
	mean := sum / float64(n)
	if mean < 0.95 || mean > 1.0 {
		t.Fatalf("Beta(100,2) mean %.3f out of [0.95,1]", mean)
	}
	// Beta(2, 100) 应明显偏 0
	sum = 0
	for i := 0; i < n; i++ {
		sum += sampleBeta(2, 100, rng)
	}
	if m := sum / float64(n); m > 0.05 || m < 0 {
		t.Fatalf("Beta(2,100) mean %.3f out of [0,0.05]", m)
	}
	// 退化参数不 panic
	_ = sampleBeta(0, 0, rng)
	_ = sampleBeta(-1, 5, rng)
}

func deterministicRand(seed int64) func() float64 {
	// 简单可复现随机源（xorshift），避免依赖全局 rand 状态
	var s uint64 = uint64(seed) | 1
	return func() float64 {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		return float64(s>>11) / float64(1<<53)
	}
}

func TestLambdaAdaptation(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.SpendWindow = 100
	cfg.Routing.BudgetPerTask["chat"] = 0.0005 // 每请求成本 ~1e-6，超预算很多倍 → λ 应大涨
	s := NewState(cfg)

	lambda0 := s.Lambda("chat")
	for i := 0; i < 200; i++ {
		s.RecordOutcome("chat", "m-a", 0.8, 1e-3, 100) // 每次 0.001 USD
	}
	lambda1 := s.Lambda("chat")
	if lambda1 <= lambda0 {
		t.Fatalf("overspend should raise lambda: %f -> %f", lambda0, lambda1)
	}

	// 反向：把预算放宽到不可能超支 → λ 应回落
	for i := 0; i < 500; i++ {
		s.RecordOutcome("chat", "m-a", 0.8, 1e-9, 100)
	}
	if s.Lambda("chat") >= lambda1 {
		t.Fatalf("underspend should lower lambda")
	}
	// 夹紧边界
	if s.Lambda("chat") < cfg.Routing.Lambda.Min {
		t.Fatalf("lambda below min")
	}
}

func TestQualityEMAAndFeedback(t *testing.T) {
	s := NewState(testConfig())
	q0 := s.Quality("code", "m-x") // 冷启动先验 0.7
	if math.Abs(q0-qualityInit) > 1e-9 {
		t.Fatalf("init quality = %f, want %f", q0, qualityInit)
	}
	for i := 0; i < 20; i++ {
		s.RecordOutcome("code", "m-x", 0.2, 1e-6, 50)
	}
	if q := s.Quality("code", "m-x"); q >= q0 {
		t.Fatalf("bad rewards should lower quality: %f -> %f", q0, q)
	}
	s.ApplyFeedback("code", "m-x", false)
	if q := s.Quality("code", "m-x"); q > s.Quality("code", "m-x") {
		t.Fatal("unreachable")
	}
	// 不同任务的同名模型状态独立
	if q := s.Quality("chat", "m-x"); math.Abs(q-qualityInit) > 1e-9 {
		t.Fatalf("task isolation broken: %f", q)
	}
}

func TestBanditConverges(t *testing.T) {
	// 好模型 90% 成功率，坏模型 10%：2000 次后采样选择应显著偏向好模型
	cfg := testConfig()
	s := NewState(cfg)
	good, bad := "m-good", "m-bad"
	for i := 0; i < 2000; i++ {
		pick := bad
		if sq, ok := s.SampleQuality("code", good, deterministicRand(int64(i)+1)); ok && sq >= 0.5 {
			pick = good
		}
		// 用伪观测模拟真实成功率
		if pick == good {
			s.RecordOutcome("code", good, 0.9, 1e-6, 10)
		} else {
			s.RecordOutcome("code", bad, 0.1, 1e-6, 10)
		}
	}
	goodQ := s.Quality("code", good)
	badQ := s.Quality("code", bad)
	if goodQ <= badQ {
		t.Fatalf("bandit failed to separate models: good=%f bad=%f", goodQ, badQ)
	}
}

func TestDecideLagrangian(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Mode = "lagrangian"
	cfg.Routing.UseBandit = new(bool) // 关闭探索，决策确定化
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())

	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hello"}}}
	dec, err := r.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier != "lagrangian" {
		t.Fatalf("tier = %s", dec.Tier)
	}
	// λ=100、两模型同质量先验：便宜的 m-a 必须胜出（cost-optimal）
	if dec.Chosen != "m-a" {
		t.Fatalf("want m-a, got %s (%s)", dec.Chosen, dec.Reason)
	}
	if dec.Reason != "cost-optimal" {
		t.Fatalf("reason = %s", dec.Reason)
	}

	// 高质量奖励让 m-b 胜出：λ 极小 → 质量主导
	for i := 0; i < 60; i++ {
		r.ReportOutcome("chat", "m-b", 1.0, 1e-6, 10)
		r.ReportOutcome("chat", "m-a", 0.1, 1e-7, 10)
	}
	r.cfg.Routing.Lambda.Min = 0.001
	dec2, _ := r.Decide(req, Hints{})
	if dec2.Chosen != "m-b" {
		t.Fatalf("quality should win with tiny lambda, got %s (%s)", dec2.Chosen, dec2.Reason)
	}
}

func TestDecideExplicit(t *testing.T) {
	cfg := testConfig()
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())
	req := &types.Request{Model: "m-b", Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}}
	dec, err := r.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier != "explicit" || dec.Chosen != "m-b" {
		t.Fatalf("explicit route broken: %+v", dec)
	}
	if _, err := r.Decide(&types.Request{Model: "ghost"}, Hints{}); err == nil {
		t.Fatal("unknown explicit model must error")
	}
}

func TestDecideHardFilters(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Mode = "lagrangian"
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())

	// 工具调用请求：m-a（无 tools）必须出局，选中的模型必须具备 tools 能力
	req := &types.Request{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
		Tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
	}
	dec, err := r.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Chosen == "m-a" {
		t.Fatalf("model without tools cap must be filtered: chose %s", dec.Chosen)
	}
	if info, _ := r.reg.Model(dec.Chosen); !info.Caps.Tools {
		t.Fatalf("chosen model lacks tools cap: %s", dec.Chosen)
	}

	// 合规组：sensitive 数据只允许 mock-local provider
	cfg2 := testConfig()
	cfg2.Routing.ComplianceGroups = map[string][]string{"sensitive": {"local"}}
	r2 := New(cfg2, newTestRegistry(cfg2), NewState(cfg2), 1, discardLogger())
	dec2, err := r2.Decide(&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}},
		Hints{DataClass: "sensitive"})
	if err != nil {
		t.Fatal(err)
	}
	if dec2.Chosen != "m-local" {
		t.Fatalf("compliance filter broken: chose %s", dec2.Chosen)
	}
	// 未定义的组名不收窄
	if _, err := r2.Decide(&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}},
		Hints{DataClass: "no-such-group"}); err != nil {
		t.Fatal("unknown data class should not filter")
	}

	// 上下文长度：超长 prompt 不能给小窗口模型
	cfg3 := testConfig()
	r3 := New(cfg3, newTestRegistry(cfg3), NewState(cfg3), 1, discardLogger())
	long := make([]byte, 50000) // ~12500 tokens：超过 m-a(4096) 与 m-local(2048)，只剩 m-b
	for i := range long {
		long[i] = 'a'
	}
	dec3, err := r3.Decide(&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: string(long)}}}, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec3.Chosen != "m-b" {
		t.Fatalf("context filter broken: chose %s", dec3.Chosen)
	}

	// 无任何可行模型
	cfg4 := testConfig()
	cfg4.Routing.ComplianceGroups = map[string][]string{"secret": {"none-exists"}}
	r4 := New(cfg4, newTestRegistry(cfg4), NewState(cfg4), 1, discardLogger())
	_, err = r4.Decide(&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}},
		Hints{DataClass: "secret"})
	if err == nil {
		t.Fatal("expected ErrNoCandidate")
	}
}

func TestDecideCascadeOrder(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Mode = "cascade"
	cfg.Cascades = map[string][]string{"code": {"m-a", "m-b"}}
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "写一个函数"}}}
	dec, err := r.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier != "cascade" || dec.Chosen != "m-a" || dec.Reason != "cascade-start" {
		t.Fatalf("cascade decide broken: %+v", dec)
	}
	if len(dec.Candidates) != 2 || dec.Candidates[0].Model != "m-a" {
		t.Fatalf("chain order broken: %+v", dec.Candidates)
	}
	// 非法 override
	if _, err := r.Decide(req, Hints{TaskOverride: "hacking"}); err == nil {
		t.Fatal("invalid task override must error")
	}
	// 合法 override 且链序保持
	dec2, err := r.Decide(req, Hints{TaskOverride: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if dec2.Task != classifier.Code || !dec2.Override {
		t.Fatalf("override broken: %+v", dec2)
	}
}

func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	cfg := testConfig()
	s := NewState(cfg)
	s.RecordOutcome("code", "m-a", 0.9, 1e-4, 42)
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	s2 := NewState(cfg)
	if err := s2.Load(path); err != nil {
		t.Fatal(err)
	}
	if q := s2.Quality("code", "m-a"); q != s.Quality("code", "m-a") {
		t.Fatalf("state roundtrip mismatch: %f vs %f", q, s.Quality("code", "m-a"))
	}
	// 损坏文件不致命
	if err := writeFile(path, "{broken"); err != nil {
		t.Fatal(err)
	}
	s3 := NewState(cfg)
	if err := s3.Load(path); err == nil {
		t.Fatal("expected parse error on corrupt state")
	}
}

func writeFile(path, content string) error {
	return osWriteFile(path, content)
}

func ExampleDecision_routeReasonHeader() {
	d := &Decision{Task: classifier.Code, Tier: "cascade", Reason: "cost-optimal", Override: false}
	fmt.Println(d.RouteReasonHeader("deepseek-chat", false))
	// Output: task=code,tier=cascade,model=deepseek-chat,upgraded=false,why=cost-optimal
}
