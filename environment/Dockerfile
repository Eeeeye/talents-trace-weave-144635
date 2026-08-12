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
    go test -buildvcs=false -race -run '^$' ./... && \
    make build && \
    command -v setpriv >/dev/null && \
    rm -rf -- bin .incident-work .reference-work demo && \
    useradd --uid 1000 --create-home --shell /bin/bash traceweave && \
    chown -R 1000:1000 /workspace/trace-weave && \
    install -d -o 1000 -g 1000 -m 0700 /solution

CMD ["/bin/bash"]
