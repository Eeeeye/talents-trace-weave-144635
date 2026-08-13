FROM golang:1.22.12-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    GOPROXY=off \
    GOSUMDB=off \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /workspace/trace-weave
COPY . .

RUN test "$(go list -m all | sed '1d' | wc -l)" -eq 0 && \
    go test -buildvcs=false ./... && \
    make build && \
    rm -rf -- bin .incident-work .reference-work demo && \
    useradd --uid 1000 --create-home --shell /bin/bash traceweave && \
    install -d -o 0 -g 0 -m 0700 /var/lib/traceweave-verifier && \
    chown -R 1000:1000 /workspace/trace-weave && \
    install -d -o 1000 -g 1000 -m 0700 /solution

CMD ["/bin/bash"]
