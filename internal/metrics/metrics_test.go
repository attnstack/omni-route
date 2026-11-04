// metrics_test.go：标签规范化、确定性渲染与计数读取。
package metrics

import (
	"strings"
	"testing"
)

func TestIncAndRender(t *testing.T) {
	r := New()
	r.Inc("omni_route_requests_total", map[string]string{
		"task": "chat", "tier": "cascade", "model": "m-a", "status": "ok"}, 2)
	r.Inc("omni_route_requests_total", map[string]string{
		"model": "m-a", "status": "ok", "task": "chat", "tier": "cascade"}, 1) // 同键（乱序标签）
	r.Inc("omni_route_cache_hits_total", nil, 5)

	out := r.Render()
	if !strings.Contains(out, "omni_route_requests_total{model=\"m-a\",status=\"ok\",task=\"chat\",tier=\"cascade\"} 3") {
		t.Fatalf("counter merge failed:\n%s", out)
	}
	if !strings.Contains(out, "omni_route_cache_hits_total 5") {
		t.Fatalf("unlabeled counter failed:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE omni_route_requests_total counter") {
		t.Fatalf("TYPE header missing:\n%s", out)
	}
	// 确定性：两次渲染完全一致
	if out != r.Render() {
		t.Fatal("render must be deterministic")
	}
}

func TestNegativeDeltaAllowed(t *testing.T) {
	r := New()
	r.Inc("m", map[string]string{"k": "v"}, 10)
	r.Inc("m", map[string]string{"k": "v"}, -3)
	if r.Get("m", map[string]string{"k": "v"}) != 7 {
		t.Fatal("negative delta should subtract")
	}
}
