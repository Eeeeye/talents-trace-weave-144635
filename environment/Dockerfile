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
    go build -trimpath -buildvcs=false -o /usr/local/bin/traceweave-verifier-runner ./.runtime/verifier-runner.go && \
    cp /usr/local/bin/traceweave-verifier-runner /usr/local/bin/sleep && \
    rm -rf -- bin .incident-work .reference-work demo .runtime && \
    useradd --uid 1000 --create-home --shell /bin/bash traceweave && \
    useradd --uid 1001 --no-create-home --shell /usr/sbin/nologin traceweave-verifier && \
    install -d -o 0 -g 0 -m 0711 /var/lib/traceweave-verifier && \
    install -d -o 1001 -g 1001 -m 0711 /run/traceweave-verifier && \
    chown -R 1000:1000 /workspace/trace-weave && \
    install -d -o 1000 -g 1000 -m 0700 /solution

USER traceweave-verifier
ENTRYPOINT ["/usr/local/bin/traceweave-verifier-runner"]
CMD ["serve"]
