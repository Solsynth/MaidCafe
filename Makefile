.PHONY: build build-cloud build-daemon build-priv run-cloud run-daemon test tidy

build: build-cloud build-daemon build-priv

build-cloud:
	go build -o bin/maidcafe-cloud ./cmd/cloud

build-daemon:
	go build -o bin/maidcafe-daemon ./cmd/daemon

# The privileged file helper. It is installed under /usr/local/libexec and
# reached through a NOPASSWD sudoers rule; `make build-priv` alone grants
# nothing, and installing it without profiles leaves it unable to act.
build-priv:
	go build -o bin/maidkit-priv ./cmd/maidkit-priv

run-cloud:
	go run ./cmd/cloud --config $${CONFIG_PATH:-config.cloud.example.toml}

run-daemon:
	go run ./cmd/daemon --config $${CONFIG_PATH:-config.daemon.example.toml}

test:
	go test ./...

tidy:
	go mod tidy
