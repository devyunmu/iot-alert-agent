.PHONY: run mcp build test vet fmt eval eval-mock

run:
	go run ./cmd/server

mcp:
	go run ./cmd/mcp

build:
	mkdir -p bin
	go build -o bin/iot-alert-agent ./cmd/server
	go build -o bin/iot-mcp ./cmd/mcp

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -w .

eval:
	EVAL_MODE=live LLM_MAX_TOKENS=$${LLM_MAX_TOKENS:-5000} LLM_TIMEOUT=$${LLM_TIMEOUT:-240s} \
		go test ./evalset -v -count=1

eval-mock:
	EVAL_MODE=mock go test ./evalset -v -count=1