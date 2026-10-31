// Package classifier 的单测：正常 / 边界 / 异常三类输入全覆盖。
// 分类器是路由的第一跳，错分会把整个决策链带偏——这里的每个用例
// 都对应一个真实会出现的请求形态。
package classifier

import (
	"testing"

	"github.com/attnstack/omni-route/internal/types"
)

func msg(role, content string) types.Message {
	return types.Message{Role: role, Content: content}
}

func TestClassifyNormal(t *testing.T) {
	cases := []struct {
		name string
		want Task
		text string
	}{
		{"代码围栏", Code, "帮我看看：\n```go\nfmt.Println(1)\n```"},
		{"函数请求", Code, "写一个函数判断回文"},
		{"翻译中文", Translation, "把这句话翻译成英文：你好"},
		{"翻译英文", Translation, "Translate into Chinese: hello world"},
		{"总结", Summarization, "请总结以下会议记录的要点"},
		{"JSON 提取", Extraction, "从文本中提取字段并以 JSON 格式返回"},
		{"推理", Reasoning, "为什么 TCP 需要 TIME_WAIT？请一步一步解释"},
		{"闲聊", Chat, "今天天气真不错，随便聊聊"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify([]types.Message{msg(types.RoleUser, c.text)})
			if got.Task != c.want {
				t.Fatalf("Classify(%q) = %s, want %s (signals=%v)", c.text, got.Task, c.want, got.Signals)
			}
		})
	}
}

func TestClassifyLastUserMessageWeighted(t *testing.T) {
	// 历史命中 translation(5分)，最后一条 user 命中 code 围栏(5×2=10分)：
	// 权重加倍后 code 必须反超——验证"当前意图优先"的设计
	msgs := []types.Message{
		msg(types.RoleUser, "把你好翻译成英文"),
		msg(types.RoleAssistant, "hello"),
		msg(types.RoleUser, "```python\nprint(1)\n```"),
	}
	got := Classify(msgs)
	if got.Task != Code {
		t.Fatalf("want code (2x weight on last user msg), got %s (signals=%v)", got.Task, got.Signals)
	}
	// 对照组：同样的信号但不加倍（中间位置）时 translation 应胜出
	msgs2 := []types.Message{
		msg(types.RoleUser, "```python\nprint(1)\n```"),
		msg(types.RoleUser, "把你好翻译成英文"),
	}
	if got := Classify(msgs2); got.Task != Translation {
		t.Fatalf("control group: want translation, got %s", got.Task)
	}
}

func TestClassifyBoundaries(t *testing.T) {
	t.Run("空内容数组", func(t *testing.T) {
		got := Classify(nil)
		if got.Task != Chat || got.Confidence != 0.25 {
			t.Fatalf("fallback expected, got %+v", got)
		}
	})
	t.Run("纯表情", func(t *testing.T) {
		got := Classify([]types.Message{msg(types.RoleUser, "🙂🙃")})
		if got.Task != Chat {
			t.Fatalf("want chat fallback, got %s", got.Task)
		}
	})
	t.Run("混合信号置信度上限", func(t *testing.T) {
		// 同时命中 code 与 extraction：置信度必须落在 [0.25, 0.95]
		got := Classify([]types.Message{msg(types.RoleUser,
			"```json\n{}\n``` 把这段代码重构一下，并以 JSON 返回结果")})
		if got.Confidence < 0.25 || got.Confidence > 0.95 {
			t.Fatalf("confidence out of range: %f", got.Confidence)
		}
	})
	t.Run("多轮历史稀释", func(t *testing.T) {
		// 历史里多次提到翻译，最后一条是代码：code 应胜出（2 倍权重 + 围栏 5 分）
		msgs := []types.Message{
			msg(types.RoleUser, "翻译一下你好"),
			msg(types.RoleAssistant, "hello"),
			msg(types.RoleUser, "```python\nprint(1)\n```"),
		}
		if got := Classify(msgs); got.Task != Code {
			t.Fatalf("want code, got %s (signals=%v)", got.Task, got.Signals)
		}
	})
}

func TestWantsJSON(t *testing.T) {
	yes := []types.Message{msg(types.RoleUser, "请提取结果并返回 JSON")}
	if !WantsJSON(yes) {
		t.Fatal("wantsJSON should be true")
	}
	no := []types.Message{msg(types.RoleUser, "今天天气怎么样")}
	if WantsJSON(no) {
		t.Fatal("wantsJSON should be false")
	}
}

func TestKeywords(t *testing.T) {
	kws := Keywords("如何在 Go 中实现 token bucket 限流算法")
	if len(kws) == 0 {
		t.Fatal("no keywords extracted")
	}
	found := false
	for _, k := range kws {
		if k == "bucket" || k == "token" || k == "限流" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected domain keyword, got %v", kws)
	}
	if kws := Keywords("的 了 是 在"); len(kws) != 0 {
		t.Fatalf("stopwords should be dropped, got %v", kws)
	}
}

func TestValid(t *testing.T) {
	if !Valid("code") || Valid("nope") {
		t.Fatal("Valid misbehaves")
	}
	if err := (Task("code")).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Task("nope")).Validate(); err == nil {
		t.Fatal("expected error for unknown task")
	}
}
