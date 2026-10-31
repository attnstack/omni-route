// Package store 是基于 SQLite 的请求日志与观测查询层。
// 写路径：内存 channel + 后台批量落盘——观测不能阻塞请求热路径，
// 也不能每条一个事务拖垮 WAL。读路径：为 dashboard 提供聚合查询。
// 依赖 modernc.org/sqlite（纯 Go，无 CGO），保证 Windows/macOS/Linux 开箱即用。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Row 是一条请求记录。表结构里的 request_id/provider/stream/status，
// 没有它们，"这条请求走了哪个 provider""客户端有没有拿全"根本无从回答。
type Row struct {
	TS               int64 // unix ms
	RequestID        string
	Task             string
	TaskConfidence   float64
	Model            string
	Provider         string
	Tier             string // direct | cascade | explicit
	Upgraded         bool
	FailoverFrom     string
	RouteReason      string
	PromptTokens     int
	CompletionTokens int
	TokensEstimated  bool
	CostUSD          float64
	SavingsUSD       float64
	LatencyMs        int64
	Stream           bool
	Status           string // ok | error | client_cancel
	Error            string
}

// Feedback 是一次用户显式反馈。
type Feedback struct {
	RequestID string
	TS        int64
	Good      bool
	Comment   string
}

// Store 是异步写 + 同步读的日志存储。
type Store struct {
	db         *sql.DB
	ch         chan Row
	dropped    atomic.Int64
	wg         sync.WaitGroup
	flushEvery time.Duration
	batchSize  int
	logger     *slog.Logger
	inMemory   bool
}

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	request_id TEXT NOT NULL,
	task TEXT,
	task_confidence REAL,
	model TEXT NOT NULL,
	provider TEXT,
	tier TEXT,
	upgraded INTEGER,
	failover_from TEXT,
	route_reason TEXT,
	prompt_tokens INTEGER,
	completion_tokens INTEGER,
	tokens_estimated INTEGER,
	cost_usd REAL,
	savings_usd REAL,
	latency_ms INTEGER,
	stream INTEGER,
	status TEXT,
	error TEXT
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_task_model ON requests(task, model);
CREATE TABLE IF NOT EXISTS feedback (
	request_id TEXT PRIMARY KEY,
	ts INTEGER NOT NULL,
	good INTEGER NOT NULL,
	comment TEXT
);
`

// Open 打开存储。磁盘库打开失败时降级为内存库：观测功能受损，
// 但网关主业务（转发）不能因为日志挂掉——可用性优先于可观测性。
func Open(path string, flushEvery time.Duration, batchSize int, logger *slog.Logger) (*Store, error) {
	if batchSize <= 0 {
		batchSize = 64
	}
	if flushEvery <= 0 {
		flushEvery = 200 * time.Millisecond
	}
	db, err := sql.Open("sqlite", path)
	if err == nil {
		err = db.Ping()
	}
	inMemory := false
	if err != nil {
		logger.Warn("sqlite open failed, falling back to in-memory store", "path", path, "error", err.Error())
		db, err = sql.Open("sqlite", ":memory:")
		if err != nil {
			return nil, fmt.Errorf("store: open in-memory: %w", err)
		}
		inMemory = true
	}
	// 单连接：SQLite 内存库是每连接独立的，连接池开第二个连接会看到
	// 一个没有 schema 的空库（CI race 模式下偶发复现）。观测层是单写者
	// goroutine + 轻量读查询，单连接没有性能代价。
	db.SetMaxOpenConns(1)
	// WAL + NORMAL：观测写入不至于拖慢，掉电最多丢最后一批日志
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: pragma: %w", err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	s := &Store{
		db:         db,
		ch:         make(chan Row, 8192),
		flushEvery: flushEvery,
		batchSize:  batchSize,
		logger:     logger,
		inMemory:   inMemory,
	}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// Log 非阻塞投递一条记录。channel 满时丢弃并计数——宁可少一条日志，
// 不能让转发请求为观测买单。
func (s *Store) Log(row Row) {
	select {
	case s.ch <- row:
	default:
		s.dropped.Add(1)
	}
}

func (s *Store) writer() {
	defer s.wg.Done()
	tick := time.NewTicker(s.flushEvery)
	defer tick.Stop()
	batch := make([]Row, 0, s.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertBatch(batch); err != nil {
			s.logger.Error("store: batch insert failed", "rows", len(batch), "error", err.Error())
		}
		batch = batch[:0]
	}
	for {
		select {
		case row, ok := <-s.ch:
			if !ok {
				// Close() 关闭 channel：冲刷剩余批次后退出
				flush()
				return
			}
			batch = append(batch, row)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-tick.C:
			if d := s.dropped.Swap(0); d > 0 {
				s.logger.Warn("store: log queue full, rows dropped", "count", d)
			}
			flush()
		}
	}
}

func (s *Store) insertBatch(rows []Row) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO requests
		(ts, request_id, task, task_confidence, model, provider, tier, upgraded, failover_from,
		 route_reason, prompt_tokens, completion_tokens, tokens_estimated, cost_usd, savings_usd,
		 latency_ms, stream, status, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.TS, r.RequestID, r.Task, r.TaskConfidence, r.Model,
			r.Provider, r.Tier, boolInt(r.Upgraded), r.FailoverFrom, r.RouteReason,
			r.PromptTokens, r.CompletionTokens, boolInt(r.TokensEstimated), r.CostUSD, r.SavingsUSD,
			r.LatencyMs, boolInt(r.Stream), r.Status, r.Error); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Aggregates 是 dashboard 一次拉取的全部统计。
type Aggregates struct {
	Totals   Totals                 `json:"totals"`
	ByModel  []GroupStat            `json:"by_model"`
	ByTask   []GroupStat            `json:"by_task"`
	Series   []Point                `json:"series"`
	InMemory bool                   `json:"in_memory"`
	Extra    map[string]interface{} `json:"-"`
}

type Totals struct {
	Requests    int64   `json:"requests"`
	Errors      int64   `json:"errors"`
	CostUSD     float64 `json:"cost_usd"`
	SavingsUSD  float64 `json:"savings_usd"`
	UpgradeRate float64 `json:"upgrade_rate"`
	AvgLatency  float64 `json:"avg_latency_ms"`
	CascadeReq  int64   `json:"cascade_requests"`
}

type GroupStat struct {
	Key         string  `json:"key"`
	Requests    int64   `json:"requests"`
	CostUSD     float64 `json:"cost_usd"`
	SavingsUSD  float64 `json:"savings_usd"`
	UpgradeRate float64 `json:"upgrade_rate"`
	AvgLatency  float64 `json:"avg_latency_ms"`
}

type Point struct {
	BucketMin  int64   `json:"bucket_min"` // unix 分钟
	Requests   int64   `json:"requests"`
	CostUSD    float64 `json:"cost_usd"`
	SavingsUSD float64 `json:"savings_usd"`
	Upgrades   int64   `json:"upgrades"`
}

// Stats 聚合全表。MVP 数据量（万级）下一次全扫可接受；
// 到十万级再按 ts 增量聚合，不为过早优化引入复杂度。
func (s *Store) Stats() (Aggregates, error) {
	agg := Aggregates{InMemory: s.inMemory}
	var err error
	if agg.Totals, err = s.totals(); err != nil {
		return agg, err
	}
	if agg.ByModel, err = s.groupBy("model"); err != nil {
		return agg, err
	}
	if agg.ByTask, err = s.groupBy("task"); err != nil {
		return agg, err
	}
	agg.Series, err = s.series(60)
	return agg, err
}

func (s *Store) totals() (Totals, error) {
	var t Totals
	row := s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status != 'ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(cost_usd), 0),
		COALESCE(SUM(savings_usd), 0),
		COALESCE(SUM(CASE WHEN tier = 'cascade' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN tier = 'cascade' AND upgraded = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(AVG(latency_ms), 0)
		FROM requests`)
	var cascadeReq, upgraded int64
	if err := row.Scan(&t.Requests, &t.Errors, &t.CostUSD, &t.SavingsUSD, &cascadeReq, &upgraded, &t.AvgLatency); err != nil {
		return t, err
	}
	t.CascadeReq = cascadeReq
	if cascadeReq > 0 {
		t.UpgradeRate = float64(upgraded) / float64(cascadeReq)
	}
	return t, nil
}

func (s *Store) groupBy(col string) ([]GroupStat, error) {
	// col 只来自程序内常量（"model"/"task"），无注入面
	q := fmt.Sprintf(`SELECT COALESCE(%s, 'unknown'), COUNT(*),
		COALESCE(SUM(cost_usd),0), COALESCE(SUM(savings_usd),0),
		COALESCE(AVG(CASE WHEN tier='cascade' THEN upgraded END),0),
		COALESCE(AVG(latency_ms),0)
		FROM requests GROUP BY %s ORDER BY COUNT(*) DESC LIMIT 50`, col, col)
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupStat
	for rows.Next() {
		var g GroupStat
		if err := rows.Scan(&g.Key, &g.Requests, &g.CostUSD, &g.SavingsUSD, &g.UpgradeRate, &g.AvgLatency); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) series(minutes int) ([]Point, error) {
	rows, err := s.db.Query(`SELECT ts/60000, COUNT(*),
		COALESCE(SUM(cost_usd),0), COALESCE(SUM(savings_usd),0),
		COALESCE(SUM(CASE WHEN tier='cascade' AND upgraded=1 THEN 1 ELSE 0 END),0)
		FROM requests GROUP BY ts/60000 ORDER BY ts/60000 DESC LIMIT ?`, minutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.BucketMin, &p.Requests, &p.CostUSD, &p.SavingsUSD, &p.Upgrades); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Recent 返回最近 n 条请求（倒序）。
func (s *Store) Recent(n int) ([]Row, error) {
	rows, err := s.db.Query(`SELECT ts, request_id, task, task_confidence, model, provider, tier,
		upgraded, failover_from, route_reason, prompt_tokens, completion_tokens, tokens_estimated,
		cost_usd, savings_usd, latency_ms, stream, status, error
		FROM requests ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var upgraded, estimated, stream int
		if err := rows.Scan(&r.TS, &r.RequestID, &r.Task, &r.TaskConfidence, &r.Model, &r.Provider,
			&r.Tier, &upgraded, &r.FailoverFrom, &r.RouteReason, &r.PromptTokens, &r.CompletionTokens,
			&estimated, &r.CostUSD, &r.SavingsUSD, &r.LatencyMs, &stream, &r.Status, &r.Error); err != nil {
			return nil, err
		}
		r.Upgraded, r.TokensEstimated, r.Stream = upgraded == 1, estimated == 1, stream == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// LookupRequest 按 request_id 查询单条记录（feedback 回写质量分时定位 task/model）。
func (s *Store) LookupRequest(requestID string) (Row, bool, error) {
	rows, err := s.db.Query(`SELECT ts, request_id, task, task_confidence, model, provider, tier,
		upgraded, failover_from, route_reason, prompt_tokens, completion_tokens, tokens_estimated,
		cost_usd, savings_usd, latency_ms, stream, status, error
		FROM requests WHERE request_id = ? ORDER BY id DESC LIMIT 1`, requestID)
	if err != nil {
		return Row{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Row{}, false, rows.Err()
	}
	var r Row
	var upgraded, estimated, stream int
	if err := rows.Scan(&r.TS, &r.RequestID, &r.Task, &r.TaskConfidence, &r.Model, &r.Provider,
		&r.Tier, &upgraded, &r.FailoverFrom, &r.RouteReason, &r.PromptTokens, &r.CompletionTokens,
		&estimated, &r.CostUSD, &r.SavingsUSD, &r.LatencyMs, &stream, &r.Status, &r.Error); err != nil {
		return Row{}, false, err
	}
	r.Upgraded, r.TokensEstimated, r.Stream = upgraded == 1, estimated == 1, stream == 1
	return r, true, nil
}

// AddFeedback 写入反馈并把对应请求标记为用户校准过的质量信号。
func (s *Store) AddFeedback(f Feedback) error {
	_, err := s.db.Exec(`INSERT INTO feedback(request_id, ts, good, comment) VALUES(?,?,?,?)
		ON CONFLICT(request_id) DO UPDATE SET ts=excluded.ts, good=excluded.good, comment=excluded.comment`,
		f.RequestID, f.TS, boolInt(f.Good), f.Comment)
	return err
}

// Close 停止写入协程并落盘剩余批次。
func (s *Store) Close() error {
	close(s.ch)
	s.wg.Wait()
	return s.db.Close()
}
