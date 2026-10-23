# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/why-slow ./cmd/why-slow && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/kubepilot-server ./cmd/kubepilot-server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /out/why-slow /usr/local/bin/why-slow
COPY --from=builder /out/kubepilot-server /usr/local/bin/kubepilot-server
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/kubepilot-server"]
