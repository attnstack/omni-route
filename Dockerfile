# 多阶段构建：静态编译 + distroless 运行时。
# 构建示例（版本注入）：
#   docker build --build-arg VERSION=v0.1.0 -t ghcr.io/attnstack/omni-route:v0.1.0 .
FROM golang:1.26-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/attnstack/omni-route/internal/buildinfo.Version=${VERSION}" \
    -o /out/omni-route ./cmd/omni-route
# /data 预建并以 nonroot(65532) 属主拷入运行时镜像：
# 新建命名卷会继承镜像内该目录的属主，否则 fresh volume 归 root，
# nonroot 进程写不了 SQLite（曾静默降级为内存库）
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/omni-route /omni-route
COPY --from=build --chown=65532:65532 /out/data /data
# 数据目录：SQLite 日志与路由状态快照（compose/运行时挂卷持久化）
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/omni-route"]
CMD ["-config", "/etc/omni-route/config.yaml"]
