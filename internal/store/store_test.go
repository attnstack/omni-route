// store 包单测：内存库上的写入/聚合/反馈闭环。
package store

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), 20*time.Millisecond, 8,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func row(requestID, task, model string, cost, savings float64, upgraded bool) Row {
	return Row{
		TS: time.Now().UnixMilli(), RequestID: requestID,
		Task: task, Model: model, Provider: "mock", Tier: "cascade",
		Upgraded: upgraded, RouteReason: "test",
		PromptTokens: 10, CompletionTokens: 20, CostUSD: cost, SavingsUSD: savings,
		LatencyMs: 42, Status: "ok",
	}
}

func TestLogAndStats(t *testing.T) {
	s := testStore(t)
	s.Log(row("r1", "code", "m-a", 0.001, 0.004, false))
	s.Log(row("r2", "code", "m-a", 0.002, 0.008, true))
	s.Log(row("r3", "chat", "m-b", 0.0005, 0.001, false))

	// 异步落盘：轮询等待批量写入完成
	deadline := time.Now().Add(3 * time.Second)
	var agg Aggregates
	var err error
	for {
		agg, err = s.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if agg.Totals.Requests == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rows not flushed, got %d", agg.Totals.Requests)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if agg.Totals.CostUSD < 0.0034 || agg.Totals.SavingsUSD < 0.012 {
		t.Fatalf("totals = %+v", agg.Totals)
	}
	if agg.Totals.UpgradeRate <= 0 {
		t.Fatalf("upgrade rate = %f", agg.Totals.UpgradeRate)
	}
	foundModel := false
	for _, g := range agg.ByModel {
		if g.Key == "m-a" {
			foundModel = true
			if g.Requests != 2 {
				t.Fatalf("m-a requests = %d", g.Requests)
			}
		}
	}
	if !foundModel {
		t.Fatal("m-a missing from by_model")
	}
}

func TestRecentAndLookup(t *testing.T) {
	s := testStore(t)
	s.Log(row("r1", "code", "m-a", 0.001, 0.004, false))
	s.Log(row("r2", "chat", "m-b", 0.002, 0.008, true))
	waitFor(t, func() bool {
		rows, _ := s.Recent(10)
		return len(rows) == 2
	})
	rows, err := s.Recent(1)
	if err != nil || len(rows) != 1 || rows[0].RequestID != "r2" {
		t.Fatalf("recent = %+v err=%v", rows, err)
	}
	got, found, err := s.LookupRequest("r1")
	if err != nil || !found || got.Task != "code" || got.Model != "m-a" {
		t.Fatalf("lookup = %+v found=%v err=%v", got, found, err)
	}
	if _, found, _ := s.LookupRequest("nope"); found {
		t.Fatal("unknown request_id should not be found")
	}
}

func TestFeedback(t *testing.T) {
	s := testStore(t)
	s.Log(row("r1", "code", "m-a", 0.001, 0, false))
	waitFor(t, func() bool {
		_, found, _ := s.LookupRequest("r1")
		return found
	})
	if err := s.AddFeedback(Feedback{RequestID: "r1", TS: time.Now().UnixMilli(), Good: false, Comment: "wrong"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddFeedback(Feedback{RequestID: "r1", TS: time.Now().UnixMilli(), Good: true}); err != nil {
		t.Fatal("upsert should tolerate repeat feedback")
	}
}

func TestInMemoryFallback(t *testing.T) {
	// 非法路径 → 降级内存库，不致命
	s, err := Open("Z:/definitely/not/a/valid/path.db", 20*time.Millisecond, 8,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("in-memory fallback failed: %v", err)
	}
	defer s.Close()
	if !s.inMemory {
		t.Fatal("expected in-memory fallback")
	}
	s.Log(row("r1", "chat", "m", 0, 0, false))
	waitFor(t, func() bool {
		agg, _ := s.Stats()
		return agg.Totals.Requests == 1
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	// 10s：并行测试负载下批量落盘可能被调度延迟，3s 在慢机上会偶发超时
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
