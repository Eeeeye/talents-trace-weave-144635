.PHONY: all build test race vet reproduce clean

all: build

build:
	mkdir -p bin
	go build -trimpath -buildvcs=false -o bin/tracegen ./cmd/tracegen
	go build -trimpath -buildvcs=false -o bin/traceweave ./cmd/traceweave
	go build -trimpath -buildvcs=false -o bin/traceinspect ./cmd/traceinspect

test:
	go test -buildvcs=false ./...

race:
	go test -buildvcs=false -race ./...

vet:
	go vet -buildvcs=false ./...

reproduce: build
	./scripts/reproduce-incident.sh

clean:
	rm -rf -- bin .incident-work demo
