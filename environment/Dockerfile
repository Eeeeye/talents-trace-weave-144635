FROM golang:1.22.12-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    GOPROXY=off \
    GOSUMDB=off \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /workspace/trace-weave
COPY . .

RUN install -d -o 0 -g 0 -m 0700 /usr/local/share/traceweave-go-cache && \
    export GOCACHE=/usr/local/share/traceweave-go-cache && \
    test "$(go list -m all | sed '1d' | wc -l)" -eq 0 && \
    go test -buildvcs=false ./... && \
    make build && \
    command -v setpriv >/dev/null && \
    rm -rf -- bin .incident-work .reference-work demo && \
    chown -R 0:0 /usr/local/share/traceweave-go-cache && \
    chmod 0700 /usr/local/share/traceweave-go-cache && \
    useradd --uid 1000 --create-home --shell /bin/bash traceweave && \
    chown -R 1000:1000 /workspace/trace-weave && \
    install -d -o 1000 -g 1000 -m 0700 /solution

CMD ["/bin/bash"]
