// router 决策路径的性能基准：分类 + 过滤 + 打分 + 采样在单请求内的开销。
// 结论预期：微秒级，与上游模型调用（百毫秒级）相比可忽略——
// 这是"网关不增加可感知延迟"主张的实测依据（README 引用此结果）。
package router

import (
	"testing"

	"github.com/attnstack/omni-route/internal/types"
)

func benchRouter(b *testing.B) *Router {
	cfg := testConfig()
	cfg.Routing.Mode = "lagrangian"
	b.Cleanup(func() {})
	return New(cfg, newTestRegistry(cfg), NewState(cfg), 42, discardLogger())
}

func BenchmarkDecide(b *testing.B) {
	r := benchRouter(b)
	req := &types.Request{
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: "you are helpful"},
			{Role: types.RoleUser, Content: "帮我写一个函数，把 CSV 解析成 JSON，注意处理转义字符和中文列名"},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Decide(req, Hints{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecideExplicit(b *testing.B) {
	r := benchRouter(b)
	req := &types.Request{
		Model:    "m-a",
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Decide(req, Hints{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecordOutcome(b *testing.B) {
	r := benchRouter(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ReportOutcome("code", "m-a", 0.8, 1e-5, 100)
	}
}

func BenchmarkSampleBeta(b *testing.B) {
	rng := deterministicRand(1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sampleBeta(12, 4, rng)
	}
}
