// Package tokens 提供零依赖的 token 估算：provider 不报 usage 时的兜底，
// 也是路由前估算上下文长度（超长任务不能路由给小窗口模型）的依据。
// 估算规则：CJK 字符约 1 rune/token，其他文本约 4 rune/token（cl100k 级别的均值近似）。
// 误差对成本结算可到 ±20%，所以所有估算结果都必须带 Estimated 标注，禁止冒充实测。
package tokens

import (
	"github.com/attnstack/omni-route/internal/types"
	"unicode"
)

// Estimate 估算一段文本的 token 数，至少返回 1（非空时）。
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	est := cjk + (other+3)/4
	if est < 1 {
		est = 1
	}
	return est
}

// EstimateMessages 估算整段对话的 prompt token 数（含角色等结构性开销的粗略补偿）。
func EstimateMessages(msgs []types.Message) int {
	total := 0
	for _, m := range msgs {
		total += Estimate(m.Content) + 4
		for _, tc := range m.ToolCalls {
			total += Estimate(tc.ArgsJSON) + 8
		}
	}
	return total
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}
