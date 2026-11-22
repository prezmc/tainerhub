VERSION ?= dev
LDFLAGS := -s -w -X github.com/prezmc/tainerhub/internal/version.Version=$(VERSION)

.PHONY: all web hub agent test images dev-web clean

all: web hub agent

web:
	cd web && npm ci && npm run build

hub: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tainerhub ./cmd/tainerhub

agent:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tainerhub-agent ./cmd/tainerhub-agent

test:
	go test -race -count=1 ./...

images:
	docker build -f Dockerfile.hub --build-arg VERSION=$(VERSION) -t ghcr.io/prezmc/tainerhub:$(VERSION) .
	docker build -f Dockerfile.agent --build-arg VERSION=$(VERSION) -t ghcr.io/prezmc/tainerhub-agent:$(VERSION) .

dev-web:
	cd web && npm run dev

clean:
	rm -rf bin web/dist/assets web/dist/index.html web/dist/favicon.svg
