# Contributing to omni-route

感谢关注。这个项目刻意保持小而硬：**两个直接依赖、单二进制、核心算法可测可讲**。
提 PR 前请先读 README 的「已知不足」——很多"明显该做"的功能是刻意不做的，先开 issue 对齐。

## 本地开发

```bash
# 环境：Go ≥ 1.22；国内建议 export GOPROXY=https://goproxy.cn,direct
go mod tidy
go test ./...          # 全部基于内置 mock provider，不需要任何 API key
go vet ./...
gofmt -l .             # 提交前应为空
```

Windows 用户：所有命令在 PowerShell / Git Bash 均可用；`scripts/sse_matrix.sh` 需要 Git Bash。

## 提交规范

1. **每个 PR 一件事**。重构与功能分开。
2. **新功能必须带测试**：正常 / 边界 / 异常三类输入；碰 HTTP 行为的加集成测试（参考 `internal/gateway/gateway_test.go` 的 `setup()`）。
3. **配置项**：所有新参数进 `internal/config`，给默认值、进 README 配置速查表；禁止在业务代码里写魔法数字。
4. **依赖**：新增第三方依赖需要在 PR 描述里说明为什么标准库不够。

## 核心算法改动（`internal/router`、`internal/cascade`）

路由打分、λ 自适应、Beta 采样、级联判别属于"改动一行、全局变向"的区域：

- 说明动机与预期影响（哪个指标会变：成本 / 准确率 / 升级率 / 延迟）；
- 跑 `go run ./cmd/omni-bench -config config.demo.yaml -dataset data/bench_tasks.jsonl`，把改动前后两张表贴进 PR；
- 保持决策可解释：`x-or-route-reason` 的语义变化必须在 README 同步。

## Benchmark 数据

`reports/` 目录存放真实 key 实测结果（`bench-<日期>.md`）。提交实测数据时注明：
provider、模型清单、日期、样本量。**禁止提交没有运行环境的编造数字**——这是本仓库的硬规矩。
