FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /eta-server ./cmd/eta-server

FROM alpine:3.22
RUN apk add --no-cache openssh-client ca-certificates && \
    addgroup -g 10001 eta && adduser -D -u 10001 -G eta eta && \
    mkdir -p /data /config /home/eta/.ssh && \
    chown -R eta:eta /data /home/eta/.ssh && chmod 700 /home/eta/.ssh
COPY --from=build /eta-server /usr/local/bin/eta-server
USER 10001:10001
ENV ETA_LISTEN=0.0.0.0:8080 ETA_DATA_DIR=/data ETA_CONFIG=/config/config.json
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["eta-server"]
