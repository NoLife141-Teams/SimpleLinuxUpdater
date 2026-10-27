# Build stage
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o webserver . \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o persistence-owner ./cmd/persistence-owner

# Runtime stage
FROM alpine:3.24 AS runtime
ENV GIN_MODE=release \
    DEBIAN_UPDATER_LISTEN_ADDR=:8080
RUN apk --no-cache upgrade \
    && apk --no-cache add ca-certificates su-exec
RUN addgroup -S app && adduser -S -G app app && mkdir -p /app /data && chown -R app:app /app /data
WORKDIR /app
COPY --from=builder --chown=app:app /app/webserver .
COPY --from=builder --chown=root:root /app/persistence-owner /usr/local/bin/persistence-owner
COPY --from=builder --chown=app:app /app/templates ./templates
COPY --from=builder --chown=app:app /app/static ./static
COPY --chown=root:root docker-healthcheck.sh /usr/local/bin/docker-healthcheck.sh
COPY --chown=root:root docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh /usr/local/bin/persistence-owner /usr/local/bin/docker-healthcheck.sh
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 CMD ["docker-healthcheck.sh"]
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["./webserver"]
