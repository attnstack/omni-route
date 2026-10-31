# omni-route 常用命令（Windows 用户可直接复制 go 命令，无需 make）

.PHONY: build test race bench run-demo tidy vet fmt

build:
	go build -o bin/omni-route$(shell go env GOEXE) ./cmd/omni-route
	go build -o bin/omni-bench$(shell go env GOEXE) ./cmd/omni-bench

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -bench=. -benchmem ./internal/router/ ./internal/classifier/

run-demo:
	go run ./cmd/omni-route -config config.demo.yaml

bench-cost:
	go run ./cmd/omni-bench -config config.demo.yaml -dataset data/bench_tasks.jsonl

tidy:
	go mod tidy

vet:
	go vet ./...

fmt:
	gofmt -l -w .
