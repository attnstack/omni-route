// Package buildinfo 承载构建期注入的版本信息。
// ldflags: -X github.com/attnstack/omni-route/internal/buildinfo.Version=vX.Y.Z
package buildinfo

// Version 由构建工具链注入；源码运行时为 "dev"。
var Version = "dev"
