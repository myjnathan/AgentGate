# AgentGate single-image deployment (pattern follows dsh-ee: the web UI
# is embedded in the Go binary, so the runtime image carries one file).
#   docker build --build-arg VERSION=$(git describe --tags --always) -t agentgate .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/agentgate ./cmd/agentgate \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ag-setup ./cmd/ag-setup

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 agentgate \
 && mkdir -p /data && chown agentgate /data
COPY --from=build /out/agentgate /usr/local/bin/agentgate
COPY --from=build /out/ag-setup /usr/local/bin/ag-setup
USER agentgate
ENV AGENTGATE_ADDR=0.0.0.0:8787 \
    AGENTGATE_DATA=/data
VOLUME /data
EXPOSE 8787
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8787/healthz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/agentgate"]
