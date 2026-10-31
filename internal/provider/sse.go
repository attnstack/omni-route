// sse.go：上游 SSE 流的统一读取器。OpenAI 与 Anthropic 的 data 行
// 都交给调用方注入的 decode 函数翻译成统一 Chunk；[DONE] / message_stop
// 由 decode 返回 io.EOF 表达。扫描器缓冲放大到 8MB 防止超长 JSON 行撑爆。
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/attnstack/omni-route/internal/types"
)

const sseMaxLineBytes = 8 << 20

// sseStream 实现统一 Chunk 流，逐事件拉取、逐 chunk 消费。
type sseStream struct {
	resp     *http.Response
	scanner  *bufio.Scanner
	decode   func(data []byte) ([]types.Chunk, error)
	queue    []types.Chunk
	closed   bool
	finished bool
}

// newSSEStream 要求 resp 已是 2xx。decode 的约定：
//   - 正常事件 → 返回 0..n 个 chunk
//   - 流结束   → 返回 io.EOF
//   - 协议错误 → 返回可读错误
func newSSEStream(resp *http.Response, decode func(data []byte) ([]types.Chunk, error)) *sseStream {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), sseMaxLineBytes)
	return &sseStream{resp: resp, scanner: sc, decode: decode}
}

// Next 返回下一个统一 chunk；流结束返回 io.EOF。
// ctx 取消会通过底层 body 读取传播（http 请求绑定了 ctx）。
func (s *sseStream) Next() (types.Chunk, error) {
	for {
		if len(s.queue) > 0 {
			c := s.queue[0]
			s.queue = s.queue[1:]
			return c, nil
		}
		if s.finished {
			return types.Chunk{}, io.EOF
		}
		for s.scanner.Scan() {
			line := s.scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue // event:/id:/注释行——payload 自带 type，无需 event 名
			}
			data := bytes.TrimSpace(line[len("data:"):])
			if len(data) == 0 {
				continue
			}
			if bytes.Equal(data, []byte("[DONE]")) {
				s.finished = true
				return types.Chunk{}, io.EOF
			}
			chunks, err := s.decode(data)
			if err != nil {
				if errors.Is(err, io.EOF) {
					s.finished = true
					return types.Chunk{}, io.EOF
				}
				s.finished = true
				return types.Chunk{}, err
			}
			if len(chunks) > 0 {
				s.queue = append(s.queue, chunks...)
			}
			if len(s.queue) > 0 {
				c := s.queue[0]
				s.queue = s.queue[1:]
				return c, nil
			}
		}
		// 扫描器退出循环：要么 body 结束，要么出错
		if err := s.scanner.Err(); err != nil {
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
				return types.Chunk{}, context.Canceled
			}
			return types.Chunk{}, err
		}
		s.finished = true
		return types.Chunk{}, io.EOF
	}
}

func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.finished = true
	return s.resp.Body.Close()
}

// doStream 发起流式 POST 并校验状态码——两个适配器的公共路径。
func doStream(ctx context.Context, client *http.Client, url string, headers map[string]string, body any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, jsonBody(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = io.CopyN(&buf, resp.Body, 4096)
		return nil, &types.UpstreamError{Status: resp.StatusCode, Body: buf.String()}
	}
	return resp, nil
}

func jsonBody(v any) io.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		// 构造失败说明内部类型有 bug，不可能是用户输入导致——panic 是诚实的
		panic(fmt.Sprintf("provider: marshal request body: %v", err))
	}
	return bytes.NewReader(b)
}

// checkStatus 把非 2xx 响应转成 UpstreamError（非流式路径共用）。
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var buf bytes.Buffer
	_, _ = io.CopyN(&buf, resp.Body, 4096)
	_ = resp.Body.Close()
	return &types.UpstreamError{Status: resp.StatusCode, Body: buf.String()}
}
