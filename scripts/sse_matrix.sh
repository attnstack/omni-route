#!/usr/bin/env bash
# SSE 兼容矩阵（curl 客户端）：验证 /v1/chat/completions 流式输出符合 OpenAI 协议。
# 用法：先启动网关（go run ./cmd/omni-route -config config.demo.yaml），再运行本脚本。
# Windows 用户请在 Git Bash 中运行。任何一步失败脚本以非零码退出。
#
# 已知坑：Git Bash 向 curl -d '中文' 传参时可能按本地代码页编码，真实模型会收到乱码。
# 含中文的请求体一律用文件传递（见 scripts/demo_request.json）：curl -d @scripts/demo_request.json
set -euo pipefail

BASE="${OMNI_BASE:-http://localhost:8080}"

echo "== 1) 非流式：OpenAI 兼容字段 =="
body=$(curl -sf "$BASE/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"mock-mid","messages":[{"role":"user","content":"你好"}]}')
echo "$body" | grep -q '"object":"chat.completion"' || { echo "FAIL: object field"; exit 1; }
echo "$body" | grep -q '"finish_reason":"stop"' || { echo "FAIL: finish_reason"; exit 1; }
echo "$body" | grep -q '"usage"' || { echo "FAIL: usage"; exit 1; }
echo "OK"

echo "== 2) 流式：SSE 事件序列 =="
stream=$(curl -sfN "$BASE/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"mock-premium","messages":[{"role":"user","content":"讲个故事"}],"stream":true}')
echo "$stream" | grep -q '"role":"assistant"' || { echo "FAIL: role chunk"; exit 1; }
echo "$stream" | grep -q '"chat.completion.chunk"' || { echo "FAIL: chunk object"; exit 1; }
echo "$stream" | grep -q '\[DONE\]' || { echo "FAIL: [DONE] terminator"; exit 1; }
echo "OK"

echo "== 3) 可解释路由头 =="
headers=$(curl -sfN -D - -o /dev/null "$BASE/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"mock-mid","messages":[{"role":"user","content":"hi"}]}')
echo "$headers" | grep -qi 'x-or-route-reason:.*task=' || { echo "FAIL: route reason header"; exit 1; }
echo "$headers" | grep -qi 'x-or-model:' || { echo "FAIL: model header"; exit 1; }
echo "$headers" | grep -qi 'x-or-cost-usd:' || { echo "FAIL: cost header"; exit 1; }
echo "OK"

echo "== 4) 任务覆盖头 =="
headers=$(curl -sfN -D - -o /dev/null "$BASE/v1/chat/completions" -H 'Content-Type: application/json' \
  -H 'x-or-task: code' -d '{"messages":[{"role":"user","content":"写一个函数"}]}')
echo "$headers" | grep -qi 'x-or-route-reason:.*task=code' || { echo "FAIL: task override"; exit 1; }
echo "$headers" | grep -qi 'override=true' || { echo "FAIL: override marker"; exit 1; }
echo "OK"

echo "ALL SSE MATRIX CHECKS PASSED"
