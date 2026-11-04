// Package cache 是响应缓存：非流式、确定性（零温）请求的精确匹配缓存。
// 刻意只做精确匹配（不做语义相似度）：语义缓存需要 embedding 依赖并引入
// "近似答案冒充原答案"的正确性风险，与本项目"可解释、可审计"的立场冲突。
// LRU + TTL 双重淘汰；键是规范化请求体的 SHA-256。
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Entry 是一次上游调用的可复用结果。
type Entry struct {
	Model     string        // 产出该回答的模型
	Response  *ResponseCopy // 统一形态的回答（含 tool_calls 与 reasoning）
	Usage     Usage         // 产出时的 token 用量
	CostUSD   float64       // 产出时的真实成本（命中时该成本未再发生，记为节省）
	CreatedAt time.Time
}

// ResponseCopy 是 types.Response 的值拷贝形态（避免外部包改动共享指针）。
type ResponseCopy struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
}

type ToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Estimated        bool
}

type element struct {
	key   string
	entry Entry
}

// Cache 是固定容量、带 TTL 的 LRU 缓存。并发安全。
// Configure 可在运行期调整上限与 TTL（配置热加载时复用同一实例，不丢已缓存内容）。
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	items map[string]*list.Element
	lru   *list.List // front = 最近使用
}

func New(ttl time.Duration, maxEntries int) *Cache {
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Cache{
		ttl:   ttl,
		max:   maxEntries,
		items: map[string]*list.Element{},
		lru:   list.New(),
	}
}

// Configure 热加载时调整 TTL 与容量；容量缩小时按 LRU 逐出。
func (c *Cache) Configure(ttl time.Duration, maxEntries int) {
	if ttl <= 0 || maxEntries <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = ttl
	c.max = maxEntries
	for c.lru.Len() > c.max {
		c.evictOldestLocked()
	}
}

// Get 返回未过期的条目并把它移到 LRU 首位。
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Entry{}, false
	}
	e := el.Value.(*element)
	if time.Since(e.entry.CreatedAt) > c.ttl {
		c.removeLocked(el)
		return Entry{}, false
	}
	c.lru.MoveToFront(el)
	return e.entry, true
}

// Put 写入条目；容量满时按 LRU 逐出最旧项。
func (c *Cache) Put(key string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*element).entry = e
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&element{key: key, entry: e})
	c.items[key] = el
	for c.lru.Len() > c.max {
		c.evictOldestLocked()
	}
}

func (c *Cache) removeLocked(el *list.Element) {
	e := el.Value.(*element)
	c.lru.Remove(el)
	delete(c.items, e.key)
}

func (c *Cache) evictOldestLocked() {
	oldest := c.lru.Back()
	if oldest == nil {
		return
	}
	c.removeLocked(oldest)
}

// Key 计算请求的缓存键。tenant 是认证 key 的哈希（多租户隔离）；
// 未启用鉴权时传空串，缓存全站共享。
// 约定：stream 标志不参与（缓存只服务非流式）；model 仅在客户端显式点名时参与——
// 自动路由下"同一个问题"应当命中，无论第一次由哪个模型作答；
// 显式点名说明调用方要的是特定模型的回答，跨模型命中是语义错误。
// 请求体的其他字段（含 temperature/max_tokens/tools 等采样参数）全部参与。
func Key(raw map[string]any, explicitModel, tenant string) string {
	if raw == nil {
		raw = map[string]any{}
	}
	canonical := make(map[string]any, len(raw))
	for k, v := range raw {
		canonical[k] = v
	}
	delete(canonical, "stream")
	if explicitModel == "" {
		delete(canonical, "model")
	}
	// Go 的 json.Marshal 对 map 按 key 排序输出——同一请求体不同字段顺序得到同一哈希
	blob, err := json.Marshal(canonical)
	if err != nil {
		// 来自 JSON 解码的 map 理论上总能再序列化；失败时退化为长度+指针无关的稳定串
		blob = []byte("unmarshalable")
	}
	sum := sha256.Sum256(blob)
	key := "v1:" + hex.EncodeToString(sum[:])
	if tenant != "" {
		// 租户隔离：不同 API key 的相同请求不共享缓存
		t := sha256.Sum256([]byte(tenant))
		key += ":t" + hex.EncodeToString(t[:8])
	}
	return key
}
