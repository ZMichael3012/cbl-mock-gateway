.PHONY: build test test-e2e vet lint fmt vuln up down

build:
	go build ./...

test:
	go test ./...

# test-e2e runs the webhook contract end-to-end scenario (tag e2e, no Docker).
test-e2e:
	go test -tags e2e -count=1 ./test/e2e/

# E2E: compose stand smoke runs follow the test/e2e/E2E.md instruction
# (scenario E2E_MockGateway_Smoke; artifacts go to test/e2e/.runs/<UTC mark>/);
# there are no PowerShell run scripts.

vet:
	go vet ./...

# lint mounts the repository; the modules come from the GitHub origin (Go proxy)
# resolves inside the container.
lint:
	docker run --rm -v "$(CURDIR)":/src -w /src golangci/golangci-lint:latest-alpine golangci-lint run ./...

fmt:
	gofmt -l .

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

up:
	docker compose -f deploy/docker-compose.yml --profile app up -d

down:
	docker compose -f deploy/docker-compose.yml down -v
