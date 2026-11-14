VERSION ?= 0.1.0
GO_IMAGE ?= golang:1.26-alpine
GO_CACHE ?= vbackuper_gomod

.PHONY: build test vet tidy fmt image up down logs

build:
	docker run --rm -v "$(CURDIR):/src" -w /src -v $(GO_CACHE):/go/pkg/mod $(GO_IMAGE) go build ./...

test:
	docker run --rm -v "$(CURDIR):/src" -w /src -v $(GO_CACHE):/go/pkg/mod $(GO_IMAGE) sh -c "go vet ./... && go test ./..."

vet:
	docker run --rm -v "$(CURDIR):/src" -w /src -v $(GO_CACHE):/go/pkg/mod $(GO_IMAGE) go vet ./...

fmt:
	docker run --rm -v "$(CURDIR):/src" -w /src -v $(GO_CACHE):/go/pkg/mod $(GO_IMAGE) gofmt -l -w .

tidy:
	docker run --rm -v "$(CURDIR):/src" -w /src -v $(GO_CACHE):/go/pkg/mod $(GO_IMAGE) go mod tidy

image:
	docker build --build-arg VERSION=$(VERSION) -t vbackuper:$(VERSION) .

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f vbackuper
