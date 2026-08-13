.PHONY: build build-cloud build-daemon run-cloud run-daemon test tidy

build: build-cloud build-daemon

build-cloud:
	go build -o bin/maidcafe-cloud ./cmd/cloud

build-daemon:
	go build -o bin/maidcafe-daemon ./cmd/daemon

run-cloud:
	go run ./cmd/cloud --config $${CONFIG_PATH:-config.cloud.example.toml}

run-daemon:
	go run ./cmd/daemon --config $${CONFIG_PATH:-config.daemon.example.toml}

test:
	go test ./...

tidy:
	go mod tidy
