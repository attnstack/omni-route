// errors.go：OpenAI 风格的错误响应体。协议兼容的一半是错误也兼容——
// 客户端 SDK 按 error.message/type 解析失败原因。
package gateway

import (
	"encoding/json"
	"net/http"
)

type openaiErrorBody struct {
	Error openaiErrorDetail `json:"error"`
}

type openaiErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := openaiErrorBody{Error: openaiErrorDetail{Message: msg, Type: typ, Code: code}}
	// 序列化纯字符串结构不会失败；即便失败也已无 fallback 可写，忽略返回值
	b, _ := json.Marshal(body)
	_, _ = w.Write(b)
}
