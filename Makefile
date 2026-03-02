APP         := blacklight
CMD         := ./cmd/blacklight
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
               -X main.version=$(VERSION) \
               -X main.commit=$(COMMIT) \
               -X main.buildTime=$(BUILD_TIME)
IMAGE       ?= blacklight
IMAGE_TAG   ?= $(VERSION)

.PHONY: build run clean docker deploy undeploy

build:
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o $(APP) $(CMD)

run: build
	./$(APP) -output web

clean:
	rm -f $(APP)

docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMAGE):$(IMAGE_TAG) .

deploy:
	kubectl apply -f deploy/

undeploy:
	kubectl delete -f deploy/ --ignore-not-found
