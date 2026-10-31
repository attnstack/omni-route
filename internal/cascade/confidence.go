// Package cascade 实现级联执行：小模型先行 → 置信度判别 → 按需升级。
// confidence.go 是判别器集合——按"每判别一次的边际成本"从低到高：
// 格式校验（确定性，0 成本）→ 启发式 judge（本地计算）→ LLM judge（一次小模型调用）
// → 自一致性（k 次采样，最贵，只给高价值任务）。
package cascade

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/attnstack/omni-route/internal/classifier"
	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

// Verdict 是一次判别的结论。Skip=true 表示判别器本次不可用（如 LLM judge 超时），
// 不参与合分——判别器挂了不能把业务请求一起挂掉。
type Verdict struct {
	Score  float64
	Pass   bool
	Reason string
	Skip   bool
}

// CheckReq 是判别所需的上下文。
type CheckReq struct {
	Messages []types.Message
	Response *types.Response
	Task     string
}

// Checker 组合多档判别器，输出单一结论。
type Checker struct {
	format    bool
	heuristic bool
	llm       *LLMJudge
	self      *SelfConsistency
	threshold func(task string) float64 // 由校准器提供在线阈值
}

func NewChecker(cfg *config.Config, exec Caller, rng *rand.Rand) *Checker {
	c := &Checker{threshold: func(string) float64 { return cfg.Cascade.Confidence.Threshold }}
	c.format = cfg.Cascade.Confidence.UseFormat
	c.heuristic = cfg.Cascade.Confidence.UseHeuristic
	if cfg.Cascade.Confidence.LLMJudge.Enabled && cfg.Cascade.Confidence.LLMJudge.Model != "" {
		c.llm = NewLLMJudge(cfg, exec, rng)
	}
	if cfg.Cascade.Confidence.SelfConsistency.Enabled {
		c.self = NewSelfConsistency(cfg, exec, rng)
	}
	return c
}

// SetThresholdFunc 注入在线阈值来源（校准器）。
// 不注入时用配置静态值——注意：这意味着校准闭环没有闭合，
// 这是"能跑"与"按设计跑"的区别所在，装配处必须显式接线。
func (c *Checker) SetThresholdFunc(fn func(task string) float64) {
	if fn != nil {
		c.threshold = fn
	}
}

// Evaluate 依次跑启用的判别器。硬失败（格式坏/截断）短路直接判负；
// 否则按信息量加权合分（LLM judge 0.5 > 启发式 0.3 > 格式 0.2，缺失的按权重
// 归一化）与在线阈值比较。不加权的话，启发式的"长度分"基线分（0.4 起）
// 会稀释 LLM judge 的低分信号——判别器合分的关键是别让弱信号淹没强信号。
func (c *Checker) Evaluate(ctx context.Context, cr CheckReq) Verdict {
	type scored struct {
		weight float64
		score  float64
		reason string
	}
	var parts []scored
	hardFail := func(score float64, reason string) Verdict {
		return Verdict{Score: score, Pass: false, Reason: reason}
	}

	if c.format {
		v := checkFormat(cr)
		if !v.Skip && !v.Pass {
			return hardFail(v.Score, "format:"+v.Reason)
		}
		if !v.Skip {
			parts = append(parts, scored{weight: 0.2, score: v.Score, reason: "format=" + fmt.Sprintf("%.2f", v.Score)})
		}
	}
	if c.heuristic {
		v := HeuristicScore(cr)
		parts = append(parts, scored{weight: 0.3, score: v.Score, reason: "heuristic=" + fmt.Sprintf("%.2f", v.Score)})
	}
	if c.llm != nil {
		v := c.llm.Check(ctx, cr)
		if !v.Skip {
			parts = append(parts, scored{weight: 0.5, score: v.Score, reason: "llm=" + fmt.Sprintf("%.2f", v.Score)})
		}
	}
	if c.self != nil {
		v := c.self.Check(ctx, cr)
		if !v.Skip {
			parts = append(parts, scored{weight: 0.5, score: v.Score, reason: "selfc=" + fmt.Sprintf("%.2f", v.Score)})
		}
	}
	if len(parts) == 0 {
		// 没有任何判别器可用：放行并明说。判别是优化，不是正确性依赖。
		return Verdict{Score: 1, Pass: true, Reason: "no-checker"}
	}
	sum, wsum := 0.0, 0.0
	var reasons []string
	for _, p := range parts {
		sum += p.weight * p.score
		wsum += p.weight
		reasons = append(reasons, p.reason)
	}
	score := sum / wsum
	threshold := c.threshold(cr.Task)
	return Verdict{
		Score:  score,
		Pass:   score >= threshold,
		Reason: strings.Join(reasons, ","),
	}
}

var fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// checkFormat 是免费的确定性判别：JSON 合法性、截断、过短。
// 它是级联经济学里 ROI 最高的一档——零成本拦掉一大类
// "小模型翻车"，且永远不会误杀（格式坏就是坏）。
func checkFormat(cr CheckReq) Verdict {
	resp := cr.Response
	if resp.FinishReason == "length" {
		return Verdict{Score: 0.2, Pass: false, Reason: "truncated"}
	}
	// JSON 任务先验结构合法性，再看长度——合法的紧凑 JSON 可以很短，
	// 长度检查若在前面会把 `{"a":1}` 这种正确答案误杀成 too-short
	if classifier.WantsJSON(cr.Messages) {
		content := strings.TrimSpace(resp.Content)
		candidate := content
		if m := fencedJSONRe.FindStringSubmatch(content); m != nil {
			candidate = strings.TrimSpace(m[1])
		}
		if !json.Valid([]byte(candidate)) {
			return Verdict{Score: 0, Pass: false, Reason: "invalid-json"}
		}
		return Verdict{Score: 1, Pass: true, Reason: "json-ok"}
	}
	content := strings.TrimSpace(resp.Content)
	if len([]rune(content)) < 8 {
		return Verdict{Score: 0.1, Pass: false, Reason: "too-short"}
	}
	// 中性下限 0.5 而非高分："格式没查出问题"本身信息量很低，
	// 若给 0.9 的高分，会在加权合分时把其他信号的合法低分托起来——
	// 判别器信号设计的基本原则：每个分量的缺省值都应该是中性而非乐观。
	return Verdict{Score: 0.5, Pass: true, Reason: "text-ok"}
}

// refusalPatterns 命中即认为回答无效——小模型最典型的"软失败"是客气的拒绝。
var refusalPatterns = []string{
	"i can't", "i cannot", "i'm sorry", "i am sorry", "as an ai",
	"无法", "抱歉，我", "对不起，我", "作为一个人工智能",
}

// HeuristicScore 是本地启发式 judge：切题度（关键词覆盖）+ 完整度（相对长度）
// 两个信号。粗糙但免费、确定、毫秒级——它是默认档位的原因，
// 同时也是网关给"直接路由"请求计算 reward 的默认质量信号。
func HeuristicScore(cr CheckReq) Verdict {
	content := strings.ToLower(cr.Response.Content)
	prompt := lastUserPrompt(cr.Messages)
	if prompt == "" {
		return Verdict{Score: 0.5, Pass: true, Reason: "no-prompt"}
	}
	score := 0.4

	// 切题度：问题关键词在回答中的覆盖率。
	// 跨语言修正：中文提问、英文/代码作答（或反向）是技术问答的常态，
	// 字面关键词命中率必然趋近 0，会把好答案系统性打成低分——
	// 真实模型实测中这是级联 7/8 次无效升级的根因。
	// 问答语言脚本不一致时改用中性分，把"切题"的判断权交给其他信号。
	qScript, aScript := scriptOf(prompt), scriptOf(cr.Response.Content)
	relevance, relMode := 0.5, "neutral"
	kws := classifier.Keywords(prompt)
	if qScript == aScript && len(kws) > 0 {
		hit := 0
		for _, k := range kws {
			if strings.Contains(content, k) {
				hit++
			}
		}
		relevance = float64(hit) / float64(len(kws))
		relMode = "hit"
	}
	score += 0.3 * relevance

	// 完整度：回答长度相对问题复杂度的比例
	runes := float64(len([]rune(strings.TrimSpace(cr.Response.Content))))
	target := float64(len([]rune(prompt))) / 6.0
	if target < 120 {
		target = 120
	}
	completeness := runes / target
	if completeness > 1 {
		completeness = 1
	}
	score += 0.3 * completeness

	for _, p := range refusalPatterns {
		if strings.Contains(content, p) {
			return Verdict{Score: 0.05, Pass: false, Reason: "refusal:" + p}
		}
	}
	if cr.Response.FinishReason == "length" {
		score -= 0.3
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return Verdict{Score: score, Pass: score >= 0.5, Reason: fmt.Sprintf("rel=%s,kws=%d", relMode, len(kws))}
}

// scriptOf 粗判文本主导语言脚本：CJK 字母占字母类字符 40% 以上视为 CJK。
// 用于切题度的跨语言中性化，不追求语言识别的精度。
func scriptOf(s string) string {
	cjk, latin := 0, 0
	for _, r := range s {
		switch {
		case r >= 0x2E80 && r <= 0x9FFF || r >= 0xAC00 && r <= 0xD7AF:
			cjk++
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			latin++
		}
	}
	if cjk*5 >= (cjk+latin)*2 { // cjk/(cjk+latin) >= 0.4
		return "cjk"
	}
	return "latin"
}

func lastUserPrompt(msgs []types.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == types.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

// ---- LLM judge ----

// Caller 是级联对执行层的最小依赖（便于测试注入假执行器）。
type Caller interface {
	Call(ctx context.Context, model string, req *types.Request) (*types.Response, error)
}

type LLMJudge struct {
	cfg  *config.Config
	exec Caller
	rng  *rand.Rand
}

func NewLLMJudge(cfg *config.Config, exec Caller, rng *rand.Rand) *LLMJudge {
	return &LLMJudge{cfg: cfg, exec: exec, rng: rng}
}

var judgeScoreRe = regexp.MustCompile(`0?\.\d+|[01](?:\.0+)?`)

// Check 用一个小模型按 rubric 打分。judge 失败 → Skip（放行业务请求）。
// judge 模型直连执行器、不走路由——否则 judge 请求本身
// 会被再分类再级联，形成无限递归。
func (j *LLMJudge) Check(ctx context.Context, cr CheckReq) Verdict {
	cfg := j.cfg.Cascade.Confidence.LLMJudge
	prompt := cr.Response.Content
	if len([]rune(prompt)) > 2000 {
		prompt = string([]rune(prompt)[:2000])
	}
	question := lastUserPrompt(cr.Messages)
	if len([]rune(question)) > 2000 {
		question = string([]rune(question)[:2000])
	}
	// temperature 固定 0：judge 本身是测量仪器，采样随机性会直接变成评分噪声
	// （实测中同一答案两次评分可差 0.2+）。测量仪器必须比被测对象更稳定。
	zero := 0.0
	req := &types.Request{
		Model: cfg.Model,
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: "You are a strict QA judge. Judge whether the ANSWER correctly and completely addresses the QUESTION. Penalize wrong facts, missing key parts, off-topic content, and broken formatting. Reply with ONLY a single number in [0,1] (1 = fully correct and complete, 0.5 = partially correct, 0 = wrong or irrelevant). No other text."},
			{Role: types.RoleUser, Content: "QUESTION:\n" + question + "\n\nANSWER:\n" + prompt},
		},
		Temperature: &zero,
		MaxTokens:   16,
	}
	jctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	resp, err := j.exec.Call(jctx, cfg.Model, req)
	if err != nil || resp == nil {
		return Verdict{Skip: true, Reason: "judge-unavailable"}
	}
	m := judgeScoreRe.FindString(strings.TrimSpace(resp.Content))
	if m == "" {
		return Verdict{Skip: true, Reason: "judge-unparseable"}
	}
	var score float64
	if _, err := fmt.Sscanf(m, "%f", &score); err != nil {
		return Verdict{Skip: true, Reason: "judge-unparseable"}
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return Verdict{Score: score, Pass: score >= 0.5, Reason: "llm-judge"}
}

// ---- 自一致性 ----

type SelfConsistency struct {
	cfg  *config.Config
	exec Caller
	rng  *rand.Rand
}

func NewSelfConsistency(cfg *config.Config, exec Caller, rng *rand.Rand) *SelfConsistency {
	return &SelfConsistency{cfg: cfg, exec: exec, rng: rng}
}

// Check 对同一问题采样 k 次，规范化后多数投票。
// 最贵的判别器，只配 reasoning 类高价值任务；一致性差说明模型自己都不确定。
func (s *SelfConsistency) Check(ctx context.Context, cr CheckReq) Verdict {
	k := s.cfg.Cascade.Confidence.SelfConsistency.K
	if k < 2 {
		k = 3
	}
	temp := 0.7
	answers := make([]string, 0, k)
	for i := 0; i < k; i++ {
		req := &types.Request{
			Model:       cr.Response.Model,
			Messages:    cr.Messages,
			Temperature: &temp,
			MaxTokens:   1024,
		}
		resp, err := s.exec.Call(ctx, cr.Response.Model, req)
		if err != nil {
			return Verdict{Skip: true, Reason: "selfc-unavailable"}
		}
		answers = append(answers, normalizeAnswer(resp.Content))
	}
	counts := map[string]int{}
	best := 0
	for _, a := range answers {
		counts[a]++
		if counts[a] > best {
			best = counts[a]
		}
	}
	ratio := float64(best) / float64(k)
	return Verdict{Score: ratio, Pass: ratio >= 0.5, Reason: fmt.Sprintf("majority=%d/%d", best, k)}
}

func normalizeAnswer(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var sb strings.Builder
	for _, r := range s {
		if strings.ContainsRune(" \t\n\r.,;:!?\"'`，。；：！？、", r) {
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
