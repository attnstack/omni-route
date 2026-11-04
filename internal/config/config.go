// Package config 负责配置的定义、加载、默认值填充、校验与热加载（mtime 轮询）。
// 所有可调参数都在这里外置——代码里禁止出现魔法数字。
package config

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/attnstack/omni-route/internal/classifier"
	"github.com/attnstack/omni-route/internal/types"

	"gopkg.in/yaml.v3"
)

// Config 是全量配置。YAML 键全部显式标注，避免字段改名造成配置静默失效。
type Config struct {
	Listen      string              `yaml:"listen"`
	APIKeys     []string            `yaml:"api_keys"`
	AdminKeyEnv string              `yaml:"admin_key_env"`
	Seed        int64               `yaml:"seed"`
	Providers   map[string]Provider `yaml:"providers"`
	Models      map[string]Model    `yaml:"models"`
	Routing     Routing             `yaml:"routing"`
	Cascades    map[string][]string `yaml:"cascades"`
	Cascade     Cascade             `yaml:"cascade"`
	Cache       CacheCfg            `yaml:"cache"`
	Reliability Reliability         `yaml:"reliability"`
	Timeouts    Timeouts            `yaml:"timeouts"`
	Store       Store               `yaml:"store"`
	Log         Log                 `yaml:"log"`
}

// Provider 是上游 provider 定义。kind 决定协议适配器：openai | anthropic | mock。
type Provider struct {
	Kind    string `yaml:"kind"`
	BaseURL string `yaml:"base_url"`
	KeyEnv  string `yaml:"key_env"`
	RPM     int    `yaml:"rpm"`
	// Anthropic 协议强制要求 max_tokens，客户端没带时用该默认值。
	DefaultMaxTokens int         `yaml:"default_max_tokens"`
	Mock             MockOptions `yaml:"mock"`
}

// MockOptions 控制 mock provider 的行为（演示 / 混沌测试 / benchmark）。
type MockOptions struct {
	LatencyMs          int     `yaml:"latency_ms"`
	JitterMs           int     `yaml:"jitter_ms"`
	StreamChunkDelayMs int     `yaml:"stream_chunk_delay_ms"`
	FailRate           float64 `yaml:"fail_rate"`
	FailFirstN         int     `yaml:"fail_first_n"`
	BadJSONRate        float64 `yaml:"bad_json_rate"`
	ShortRate          float64 `yaml:"short_rate"`
}

// Model 是模型条目：归属 provider、单价（USD/1M tokens）、能力、mock 档位。
type Model struct {
	Provider string     `yaml:"provider"`
	PriceIn  float64    `yaml:"price_in"`
	PriceOut float64    `yaml:"price_out"`
	Caps     types.Caps `yaml:"caps"`
	MockTier string     `yaml:"mock_tier"` // fast | mid | premium
}

type Routing struct {
	Mode string `yaml:"mode"` // direct | lagrangian | cascade
	// DefaultModel 是 direct 模式与未知任务类型的兜底模型。
	DefaultModel string            `yaml:"default_model"`
	TaskDefaults map[string]string `yaml:"task_defaults"`
	// BudgetPerTask：每 1k 请求的预算（USD）。spend_window 滑动窗口内实际花费
	// 超过 budget*(window/1000) 时 λ 上调，路由向便宜模型倾斜。
	BudgetPerTask    map[string]float64            `yaml:"budget_per_task"`
	Baseline         map[string]string             `yaml:"baseline"`
	ComplianceGroups map[string][]string           `yaml:"compliance_groups"`
	LatencyBudgetMs  int                           `yaml:"latency_budget_ms"`
	SpendWindow      int                           `yaml:"spend_window"`
	Lambda           LambdaCfg                     `yaml:"lambda"`
	LatencyWeight    float64                       `yaml:"latency_weight"`
	UseBandit        *bool                         `yaml:"use_bandit"` // nil = 默认开
	BanditPriors     map[string][2]float64         `yaml:"-"`          // 预留：冷启动先验
	States           map[string]map[string]float64 `yaml:"-"`          // 预留：状态持久化模板
}

// LambdaCfg 是拉格朗日乘子的自适应参数。
// λ 的量纲是"质量分/美元"：Q∈[0,1]，单请求成本通常 1e-4~1e-2 USD，
// 因此 init=50 起步时 λC 与 Q 同量级，不会一开始就压死质量。
type LambdaCfg struct {
	Init float64 `yaml:"init"`
	Min  float64 `yaml:"min"`
	Max  float64 `yaml:"max"`
	Eta  float64 `yaml:"eta"`
}

type Cascade struct {
	MaxUpgrades       int        `yaml:"max_upgrades"`
	MaxLatencyMs      int        `yaml:"max_latency_ms"`
	TargetUpgradeRate float64    `yaml:"target_upgrade_rate"`
	CalibrationStep   float64    `yaml:"calibration_step"`
	CalibrationWindow int        `yaml:"calibration_window"`
	Confidence        Confidence `yaml:"confidence"`
}

type Confidence struct {
	Threshold       float64            `yaml:"threshold"`
	UseFormat       bool               `yaml:"use_format"`
	UseHeuristic    bool               `yaml:"use_heuristic"`
	LLMJudge        LLMJudgeCfg        `yaml:"llm_judge"`
	SelfConsistency SelfConsistencyCfg `yaml:"self_consistency"`
}

type LLMJudgeCfg struct {
	Enabled    bool    `yaml:"enabled"`
	Model      string  `yaml:"model"`
	SampleRate float64 `yaml:"sample_rate"`
	TimeoutMs  int     `yaml:"timeout_ms"`
}

type SelfConsistencyCfg struct {
	Enabled bool `yaml:"enabled"`
	K       int  `yaml:"k"`
}

// CacheCfg 是响应缓存配置。精确匹配、仅非流式；zero_temp 模式只缓存
// 显式 temperature=0 的请求（temperature 缺省时各 provider 语义不同，不缓存）。
type CacheCfg struct {
	Enabled    bool   `yaml:"enabled"`
	Mode       string `yaml:"mode"` // zero_temp | always
	TTLSeconds int    `yaml:"ttl_seconds"`
	MaxEntries int    `yaml:"max_entries"`
}

type Reliability struct {
	Retries       int     `yaml:"retries"`
	BackoffBaseMs int     `yaml:"backoff_base_ms"`
	Breaker       Breaker `yaml:"breaker"`
}

type Breaker struct {
	WindowSec  int     `yaml:"window_sec"`
	MinSamples int     `yaml:"min_samples"`
	ErrorRate  float64 `yaml:"error_rate"`
	OpenSec    int     `yaml:"open_sec"`
}

type Timeouts struct {
	ConnectMs   int `yaml:"connect_ms"`
	FirstByteMs int `yaml:"first_byte_ms"`
	TotalMs     int `yaml:"total_ms"`
}

type Store struct {
	Path            string `yaml:"path"`
	FlushIntervalMs int    `yaml:"flush_interval_ms"`
	BatchSize       int    `yaml:"batch_size"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Load 读取并校验配置。校验失败返回聚合错误，避免"启动一半才发现配置错"。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ApplyDefaults 填补零值字段。程序内直接构造 Config（测试/工具链）也必须调用它，
// 否则超时等参数为 0 会产生"立即超时"这类难排查的行为。
func (c *Config) ApplyDefaults() {
	setStr := func(s *string, def string) {
		if *s == "" {
			*s = def
		}
	}
	setInt := func(i *int, def int) {
		if *i == 0 {
			*i = def
		}
	}
	setF64 := func(f *float64, def float64) {
		if *f == 0 {
			*f = def
		}
	}
	setStr(&c.Listen, ":8080")
	setStr(&c.Log.Level, "info")
	setStr(&c.Log.Format, "text")
	setStr(&c.Store.Path, "data/omni.db")
	setInt(&c.Store.FlushIntervalMs, 200)
	setInt(&c.Store.BatchSize, 64)
	setStr(&c.Routing.Mode, "cascade")
	setInt(&c.Routing.SpendWindow, 1000)
	setInt(&c.Routing.LatencyBudgetMs, 30000)
	if c.Routing.Lambda.Init == 0 {
		c.Routing.Lambda.Init = 50
	}
	if c.Routing.Lambda.Min == 0 {
		c.Routing.Lambda.Min = 1
	}
	if c.Routing.Lambda.Max == 0 {
		c.Routing.Lambda.Max = 5000
	}
	if c.Routing.Lambda.Eta == 0 {
		c.Routing.Lambda.Eta = 0.05
	}
	if c.Routing.UseBandit == nil {
		t := true
		c.Routing.UseBandit = &t
	}
	setInt(&c.Cascade.MaxUpgrades, 2)
	setInt(&c.Cascade.MaxLatencyMs, 15000)
	setF64(&c.Cascade.TargetUpgradeRate, 0.30)
	setF64(&c.Cascade.CalibrationStep, 0.05)
	setInt(&c.Cascade.CalibrationWindow, 50)
	setF64(&c.Cascade.Confidence.Threshold, 0.60)
	setInt(&c.Reliability.Retries, 2)
	setInt(&c.Reliability.BackoffBaseMs, 200)
	setInt(&c.Reliability.Breaker.WindowSec, 300)
	setInt(&c.Reliability.Breaker.MinSamples, 20)
	setF64(&c.Reliability.Breaker.ErrorRate, 0.20)
	setInt(&c.Reliability.Breaker.OpenSec, 30)
	setInt(&c.Timeouts.ConnectMs, 5000)
	setInt(&c.Timeouts.FirstByteMs, 60000)
	setInt(&c.Timeouts.TotalMs, 300000)
	if c.Cascade.Confidence.LLMJudge.SampleRate == 0 {
		c.Cascade.Confidence.LLMJudge.SampleRate = 0.05
	}
	if c.Cascade.Confidence.LLMJudge.TimeoutMs == 0 {
		c.Cascade.Confidence.LLMJudge.TimeoutMs = 8000
	}
	if c.Cascade.Confidence.SelfConsistency.K == 0 {
		c.Cascade.Confidence.SelfConsistency.K = 3
	}
	if c.Cache.Mode == "" {
		c.Cache.Mode = "zero_temp"
	}
	if c.Cache.TTLSeconds == 0 {
		c.Cache.TTLSeconds = 3600
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = 4096
	}
}

// Validate 聚合所有配置错误。启动时全量校验一次，换来运行期不用防御配置。
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	switch c.Routing.Mode {
	case "direct", "lagrangian", "cascade":
	default:
		add("routing.mode %q not in (direct|lagrangian|cascade)", c.Routing.Mode)
	}

	for name, p := range c.Providers {
		switch p.Kind {
		case "openai", "anthropic", "mock":
		default:
			add("providers.%s.kind %q not in (openai|anthropic|mock)", name, p.Kind)
		}
		if p.Kind != "mock" && p.BaseURL == "" {
			add("providers.%s.base_url is required", name)
		}
	}

	for name, m := range c.Models {
		if _, ok := c.Providers[m.Provider]; !ok {
			add("models.%s: provider %q not defined", name, m.Provider)
		}
		switch m.MockTier {
		case "fast", "mid", "premium", "":
		default:
			add("models.%s.mock_tier %q not in (fast|mid|premium)", name, m.MockTier)
		}
	}

	if c.Routing.DefaultModel != "" {
		if _, ok := c.Models[c.Routing.DefaultModel]; !ok {
			add("routing.default_model %q not defined", c.Routing.DefaultModel)
		}
	}
	for task, model := range c.Routing.TaskDefaults {
		if !classifier.Valid(task) {
			add("routing.task_defaults: unknown task %q", task)
		}
		if _, ok := c.Models[model]; !ok {
			add("routing.task_defaults.%s: model %q not defined", task, model)
		}
	}
	for task := range c.Routing.BudgetPerTask {
		if !classifier.Valid(task) {
			add("routing.budget_per_task: unknown task %q", task)
		}
	}
	for task, model := range c.Routing.Baseline {
		if !classifier.Valid(task) {
			add("routing.baseline: unknown task %q", task)
		}
		if _, ok := c.Models[model]; !ok {
			add("routing.baseline.%s: model %q not defined", task, model)
		}
	}
	for group, providers := range c.Routing.ComplianceGroups {
		for _, p := range providers {
			if _, ok := c.Providers[p]; !ok {
				add("routing.compliance_groups.%s: provider %q not defined", group, p)
			}
		}
	}

	for task, chain := range c.Cascades {
		if !classifier.Valid(task) {
			add("cascades: unknown task %q", task)
		}
		if len(chain) == 0 {
			add("cascades.%s: empty chain", task)
		}
		if len(chain) > c.Cascade.MaxUpgrades+1 {
			add("cascades.%s: chain length %d exceeds max_upgrades+1 (%d)", task, len(chain), c.Cascade.MaxUpgrades+1)
		}
		for _, model := range chain {
			if _, ok := c.Models[model]; !ok {
				add("cascades.%s: model %q not defined", task, model)
			}
		}
	}

	if c.Routing.DefaultModel == "" && c.Routing.Mode != "cascade" && len(c.Routing.TaskDefaults) == 0 {
		add("routing.default_model is required when mode != cascade")
	}
	if c.Cascade.Confidence.LLMJudge.Enabled && c.Cascade.Confidence.LLMJudge.Model != "" {
		if _, ok := c.Models[c.Cascade.Confidence.LLMJudge.Model]; !ok {
			add("cascade.confidence.llm_judge.model %q not defined", c.Cascade.Confidence.LLMJudge.Model)
		}
	}

	switch c.Cache.Mode {
	case "zero_temp", "always":
	default:
		add("cache.mode %q not in (zero_temp|always)", c.Cache.Mode)
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: %d error(s):\n  - %s", len(errs), join(errs, "\n  - "))
	}
	return nil
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// Holder 用 atomic.Value 支持运行期整包替换配置（热加载），
// 读方拿到的引用永远是完整一致的一份快照。
type Holder struct {
	v atomic.Value // *Config
}

func NewHolder(cfg *Config) *Holder {
	h := &Holder{}
	h.v.Store(cfg)
	return h
}

func (h *Holder) Get() *Config {
	return h.v.Load().(*Config)
}

func (h *Holder) Set(cfg *Config) {
	h.v.Store(cfg)
}

// Watch mtime 轮询热加载（跨平台、零依赖）。每次变更回调传入新配置；
// 校验失败时保留旧配置并上报错误——配置错了不能让网关挂掉。
func Watch(path string, interval time.Duration, onChange func(*Config, error)) func() {
	stop := make(chan struct{})
	go func() {
		var lastMod time.Time
		if st, err := os.Stat(path); err == nil {
			lastMod = st.ModTime()
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				st, err := os.Stat(path)
				if err != nil || !st.ModTime().After(lastMod) {
					continue
				}
				lastMod = st.ModTime()
				cfg, err := Load(path)
				onChange(cfg, err)
			}
		}
	}()
	// 幂等停止：main 里 defer 与显式调用会各触发一次，双重 close 会 panic
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}
