// tokens 包单测：中英混合估算与边界。
package tokens

import (
	"testing"

	"github.com/attnstack/omni-route/internal/types"
)

func TestEstimate(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abcd", 1},   // 4 字符 → 1
		{"abcde", 2},  // 5 字符 → ceil(5/4)=2
		{"你好世界", 4},   // CJK 1 rune/token
		{"你好世界ab", 5}, // 4 CJK + 2 latin → ceil(2/4)=1
	}
	for _, c := range cases {
		if got := Estimate(c.in); got != c.want {
			t.Errorf("Estimate(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// 重复调用必须稳定（幂等）
	if Estimate("hello world") != Estimate("hello world") {
		t.Fatal("non-deterministic estimate")
	}
}

func TestEstimateMessages(t *testing.T) {
	msgs := []types.Message{
		{Role: types.RoleSystem, Content: "be brief"},
		{Role: types.RoleUser, Content: "你好"},
	}
	// 每条消息 +4 结构开销
	want := Estimate("be brief") + 4 + Estimate("你好") + 4
	if got := EstimateMessages(msgs); got != want {
		t.Fatalf("EstimateMessages = %d, want %d", got, want)
	}
	if EstimateMessages(nil) != 0 {
		t.Fatal("empty messages should be 0")
	}
}
