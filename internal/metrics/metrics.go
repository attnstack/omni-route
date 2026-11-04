// Package metrics 是零依赖的 Prometheus 文本格式指标注册表。
// 刻意不引入 client_golang：网关的卖点之一是"仅两个直接依赖"，
// 而这里需要的只是带标签的计数器 + 确定性文本渲染，百行以内。
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Default 是进程级注册表（单进程网关共享一份）。
var Default = New()

type labelKey struct {
	name   string
	labels string // 规范化后的 "k=v,k2=v2"（已排序）
}

// Registry 保存带标签的计数器。并发安全；Inc 是 O(1)，Render 是 O(n log n)。
type Registry struct {
	mu       sync.Mutex
	counters map[labelKey]int64
}

func New() *Registry {
	return &Registry{counters: map[labelKey]int64{}}
}

// Inc 累加一个带标签计数器。labels 为 nil 或空表示无标签样本。
func (r *Registry) Inc(name string, labels map[string]string, delta int64) {
	r.IncBy(name, labels, delta)
}

func (r *Registry) IncBy(name string, labels map[string]string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[labelKey{name: name, labels: canonicalLabels(labels)}] += delta
}

// Get 读取一个计数器（测试与 /api/stats 用）。
func (r *Registry) Get(name string, labels map[string]string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[labelKey{name: name, labels: canonicalLabels(labels)}]
}

func canonicalLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return strings.Join(parts, ",")
}

// Render 输出 Prometheus 文本格式（按指标族分组、组内按标签排序，输出确定）。
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 按指标名聚合所有样本行
	families := map[string][]string{}
	names := map[string]bool{}
	for k, v := range r.counters {
		line := renderLine(k, v)
		families[k.name] = append(families[k.name], line)
		names[k.name] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var sb strings.Builder
	for _, name := range sorted {
		lines := families[name]
		sort.Strings(lines)
		fmt.Fprintf(&sb, "# HELP %s %s\n", name, helpText(name))
		fmt.Fprintf(&sb, "# TYPE %s counter\n", name)
		for _, l := range lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func renderLine(k labelKey, v int64) string {
	if k.labels == "" {
		return fmt.Sprintf("%s %d", k.name, v)
	}
	// labels 已是 k="v" 逗号连接的规范形态，直接嵌入
	return fmt.Sprintf("%s{%s} %d", k.name, k.labels, v)
}

// helpText 集中维护各指标的描述，避免散落。
func helpText(name string) string {
	switch name {
	case "omni_route_requests_total":
		return "Chat completions served by the gateway."
	case "omni_route_prompt_tokens_total":
		return "Prompt tokens reported or estimated across upstream calls."
	case "omni_route_completion_tokens_total":
		return "Completion tokens reported or estimated across upstream calls."
	case "omni_route_cost_micro_usd_total":
		return "Cumulative upstream cost in micro-US dollars (1e-6 USD)."
	case "omni_route_savings_micro_usd_total":
		return "Cumulative savings vs the per-task baseline model, in micro-US dollars."
	case "omni_route_cascade_upgrades_total":
		return "Cascade requests whose first-tier answer was rejected and upgraded."
	case "omni_route_cache_hits_total":
		return "Requests served from the response cache."
	case "omni_route_cache_misses_total":
		return "Requests that went to an upstream provider (cache miss or bypass)."
	default:
		return name
	}
}
