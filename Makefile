.PHONY: test vet run redis up tidy fmt

# Unit tests. Cache tests use miniredis, so a running Redis is not required.
test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

# Redis only, for `make run` on the host. Does not start the gateway.
redis:
	docker compose up -d redis

# Build and start Redis and RediGate. Requires UPSTREAM_BASE_URL. See .env.example.
up:
	docker compose up --build

# Run the gateway on the host. Requires Redis and UPSTREAM_BASE_URL.
run:
	go run ./cmd/redigate
