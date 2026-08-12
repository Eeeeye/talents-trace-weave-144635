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
    command -v setpriv >/dev/null && \
    rm -rf -- bin .incident-work .reference-work demo && \
    useradd --uid 10001 --create-home --shell /bin/bash traceweave && \
    chown -R 10001:10001 /workspace/trace-weave

CMD ["/bin/bash"]
