// retry.go：重试判定与指数退避 + 抖动。抖动避免上游故障恢复瞬间
// 所有等待者同时重试造成二次雪崩（thundering herd）。
package reliability

import (
	"math/rand"
	"time"

	"github.com/attnstack/omni-route/internal/types"
)

// Retryable 判断错误是否值得重试：上游 5xx/429、网络错误、超时。
// 4xx 其余状态是请求本身的问题，重试只会重复失败。
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if ue, ok := err.(*types.UpstreamError); ok {
		return ue.Retryable()
	}
	// 网络层错误（连接拒绝、EOF、超时）默认可重试
	return true
}

// Backoff 计算 base*2^attempt + 均匀抖动的退避时长。
func Backoff(attempt int, baseMs int, rng *rand.Rand) time.Duration {
	if baseMs <= 0 {
		baseMs = 200
	}
	d := time.Duration(baseMs) * time.Millisecond
	for i := 0; i < attempt && d < 10*time.Second; i++ {
		d *= 2
	}
	if rng != nil {
		d += time.Duration(rng.Int63n(int64(d/4) + 1))
	}
	return d
}
