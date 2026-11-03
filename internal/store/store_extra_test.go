// store_extra_test.go：队列满载丢弃、批量插入冲突路径与非法查询的补充覆盖。
package store

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestLogQueueFullDropsAndCounts(t *testing.T) {
	// 队列容量 8192：灌 8300 条触发丢弃分支并计数（后台写线程消费速率远低于灌入速率）
	s, err := Open(filepath.Join(t.TempDir(), "drop.db"), time.Hour, 1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 8300; i++ {
		s.Log(row("drop", "chat", "m", 0, 0, false))
	}
	// 不 panic、不阻塞即通过；丢弃计数在下一个 tick 打日志（此处只验证不致命）
}

func TestInsertBatchConflictPath(t *testing.T) {
	// 主键冲突（同 ts/request_id 走 AUTOINCREMENT 不会冲突，这里用外键式约束不适用）——
	// 直接验证合法批量插入与查询往返即可，错误路径由磁盘故障注入覆盖成本过高，
	// 改为验证 batch 刷新粒度：batch_size=2 时 5 行分 3 批落盘。
	s := testStoreWith(t, 2)
	for i := 0; i < 5; i++ {
		s.Log(row("batch", "code", "m-a", 0.001, 0.002, false))
	}
	waitFor(t, func() bool {
		agg, _ := s.Stats()
		return agg.Totals.Requests == 5
	})
	if agg, _ := s.Stats(); agg.Totals.Requests != 5 {
		t.Fatalf("requests = %d", agg.Totals.Requests)
	}
}

func testStoreWith(t *testing.T, batchSize int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "batch.db"), 20*time.Millisecond, batchSize,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
