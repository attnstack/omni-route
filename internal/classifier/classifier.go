// Package classifier 是零依赖的规则版任务分类器（MVP，的第①级实现）。
// 设计取舍：用双语关键词 + 结构信号（代码围栏、JSON 要求）打分取最高，
// 不上模型——分类错误有显式覆盖（x-or-task 头）兜底，规则版的可解释性
// 和 0 成本在 MVP 阶段收益最高。置信度 = 最高分 / 总分，只作参考不作为硬边界。
package classifier

import (
	"fmt"
	"sort"
	"strings"

	"github.com/attnstack/omni-route/internal/types"
)

// Task 是六类任务类型。新增类型必须同步 config 校验与路由配置键。
type Task string

const (
	Code          Task = "code"
	Reasoning     Task = "reasoning"
	Summarization Task = "summarization"
	Translation   Task = "translation"
	Extraction    Task = "extraction"
	Chat          Task = "chat"
)

// All 返回全部任务类型（config 校验与文档共用）。
func All() []Task {
	return []Task{Code, Reasoning, Summarization, Translation, Extraction, Chat}
}

// Valid 报告 t 是否为已知任务类型。
func Valid(t string) bool {
	for _, task := range All() {
		if string(task) == t {
			return true
		}
	}
	return false
}

// Result 是分类结果。Signals 记录命中了哪些信号——可解释路由的一部分：
// 响应头与日志里能看到"为什么被判成 code"。
type Result struct {
	Task       Task
	Confidence float64
	Signals    []string
}

// 关键词权重表。双语覆盖；权重是拍定的先验（代码围栏 5 分几乎一锤定音），
// 靠测试与反馈闭环校准，不追求一次调对。
var keywordRules = []struct {
	task   Task
	weight float64
	terms  []string
	signal string
}{
	{Code, 5, []string{"```", "function ", "def ", "class ", "import ", "const ", "let ", "var ", "=> ", "printf", "println", "console.log", "nil", "null;", "};", "{\n", "SELECT ", "git "}, "structure"},
	{Code, 3, []string{"报错", "堆栈", "stack trace", "compile", "编译", "bug", "debug", "调试", "refactor", "重构", "unit test", "单测", "正则"}, "keyword"},
	{Code, 2, []string{"写一个函数", "实现一个", "write a function", "write a script", "write a program", "fix this", "这段代码", "the following code", "api 接口"}, "phrase"},
	{Reasoning, 3, []string{"step by step", "一步一步", "一步步", "推导", "证明", "prove", "derive", "推理", "think through", "逻辑"}, "keyword"},
	{Reasoning, 2, []string{"为什么", "why ", "how come", "解释一下为什么", "solve", "求解", "多少种", "概率", "矛盾"}, "phrase"},
	{Summarization, 4, []string{"总结", "概括", "summarize", "summary of", "tl;dr", "tldr", "摘要", "要点"}, "keyword"},
	{Summarization, 2, []string{"这段文字", "这篇文章", "以下内容", "the following text", "the article", "the passage"}, "phrase"},
	{Translation, 5, []string{"翻译", "翻成", "译成", "translate", "translation of", "in english", "into english", "into chinese", "into japanese"}, "keyword"},
	{Extraction, 4, []string{"提取", "抽取", "extract", "parse the following", "解析以下"}, "keyword"},
	{Extraction, 3, []string{"json", "jsonl", "yaml", "csv", "字段", "fields", "schema"}, "keyword"},
}

// stopWords 是相关性打分时剔除的虚词（中英混合高频词，出现与否没有信息量）。
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true, "of": true,
	"to": true, "in": true, "on": true, "and": true, "or": true, "for": true, "with": true,
	"this": true, "that": true, "it": true, "be": true, "as": true, "at": true, "by": true,
	"的": true, "了": true, "是": true, "在": true, "我": true, "有": true, "和": true,
	"就": true, "不": true, "人": true, "都": true, "一个": true, "上": true,
}

// Classify 对整段对话分类。最后一条 user 消息权重加倍——
// 用户的"当前意图"比历史消息更能代表任务类型。
func Classify(messages []types.Message) Result {
	scores := map[Task]float64{}
	signals := map[Task][]string{}

	for i, m := range messages {
		text := strings.ToLower(m.Content)
		if text == "" {
			continue
		}
		weight := 1.0
		if m.Role == types.RoleUser && i == len(messages)-1 {
			weight = 2.0
		}
		for _, rule := range keywordRules {
			for _, term := range rule.terms {
				if strings.Contains(text, term) {
					scores[rule.task] += rule.weight * weight
					signals[rule.task] = append(signals[rule.task], term)
					break // 同一条规则内命中一次即计分，避免长文本刷分
				}
			}
		}
		// 结构信号：JSON 输出要求是 extraction 的强信号
		if strings.Contains(text, "json") &&
			(strings.Contains(text, "输出") || strings.Contains(text, "返回") || strings.Contains(text, "return") || strings.Contains(text, "format") || strings.Contains(text, "extract")) {
			scores[Extraction] += 3 * weight
			signals[Extraction] = append(signals[Extraction], "json-required")
		}
	}

	top, topScore := Chat, 0.0
	total := 0.0
	for task, sc := range scores {
		total += sc
		if sc > topScore {
			top, topScore = task, sc
		}
	}
	if topScore == 0 {
		// 无任何信号：兜底 chat。置信度给低值，提醒使用者这是猜测。
		return Result{Task: Chat, Confidence: 0.25, Signals: []string{"fallback"}}
	}
	conf := topScore / total
	if conf < 0.25 {
		conf = 0.25
	}
	if conf > 0.95 {
		conf = 0.95
	}
	sort.Strings(signals[top])
	return Result{Task: top, Confidence: conf, Signals: signals[top]}
}

// WantsJSON 报告这段对话是否要求 JSON 输出——级联置信度判别的
// 格式校验（免费且确定性）依赖它判断"结构对不对"。
func WantsJSON(messages []types.Message) bool {
	r := Classify(messages)
	if r.Task == Extraction {
		return true
	}
	for _, m := range messages {
		t := strings.ToLower(m.Content)
		if strings.Contains(t, "json") && (strings.Contains(t, "输出") || strings.Contains(t, "返回") ||
			strings.Contains(t, "return") || strings.Contains(t, "format") || strings.Contains(t, "extract") ||
			strings.Contains(t, "```json")) {
			return true
		}
	}
	return false
}

// Keywords 抽取问题关键词（拉丁词 + 中文二元组），供启发式 judge 做
// "回答是否切题"的确定性打分。导出以便单测覆盖分词边界。
func Keywords(text string) []string {
	var out []string
	var cur []rune
	var cjkRun []rune
	flush := func() {
		if len(cur) >= 3 {
			w := strings.ToLower(string(cur))
			if !stopWords[w] {
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	drainCJK := func() {
		for i := 0; i+1 < len(cjkRun); i++ {
			bg := string(cjkRun[i : i+2])
			if !stopWords[bg] {
				out = append(out, bg)
			}
		}
		cjkRun = cjkRun[:0]
	}
	for _, r := range text {
		switch {
		case isLatin(r):
			cur = append(cur, r)
			drainCJK()
		case isCJKChar(r):
			flush()
			cjkRun = append(cjkRun, r)
		default:
			flush()
			drainCJK()
		}
	}
	flush()
	drainCJK()
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func isLatin(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

func isCJKChar(r rune) bool {
	return r >= 0x2E80 && r <= 0x9FFF || r >= 0xAC00 && r <= 0xD7AF
}

// String 使 Task 可直接用于日志与 fmt。
func (t Task) String() string { return string(t) }

// Validate 用在 config 加载处兜底。
func (t Task) Validate() error {
	if !Valid(string(t)) {
		return fmt.Errorf("unknown task type %q", t)
	}
	return nil
}
