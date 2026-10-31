// Package web 内嵌 dashboard 静态资源。构建期 embed 进二进制，
// 保持"单文件部署"的产品承诺。
package web

import _ "embed"

//go:embed dashboard/index.html
var DashboardHTML []byte
