BINARY := bin/cli-ai-gateway
CLIENT_BINARY := bin/cli-ai-client

.PHONY: build run start stop restart status logs test test-verbose lint fmt clean docker-build docker-up docker-down docker-restart docker-logs

build:
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/gateway
	go build -o $(CLIENT_BINARY) ./cmd/example-client

run: build
	$(BINARY) -config configs/config.example.yaml

start: build
	./scripts/gateway.sh start

stop:
	./scripts/gateway.sh stop

restart: build
	./scripts/gateway.sh restart

status:
	./scripts/gateway.sh status

logs:
	./scripts/gateway.sh logs

test:
	go test -race ./...

test-verbose:
	go test -race -v ./...

lint:
	go vet ./...

fmt:
	gofmt -w cmd internal pkg tests

clean:
	./scripts/gateway.sh stop >/dev/null 2>&1 || true
	rm -rf bin data

docker-build:
	docker compose build

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

docker-restart:
	docker compose restart gateway

docker-logs:
	docker compose logs -f gateway
