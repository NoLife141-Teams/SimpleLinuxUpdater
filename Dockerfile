# Build stage
FROM golang:1.26.2-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o webserver .

# Runtime stage
FROM alpine:3.22
RUN apk --no-cache add ca-certificates
RUN addgroup -S app && adduser -S -G app app && mkdir -p /app /data && chown -R app:app /app /data
WORKDIR /app
COPY --from=builder --chown=app:app /app/webserver .
COPY --from=builder --chown=app:app /app/templates ./templates
COPY --from=builder --chown=app:app /app/static ./static
VOLUME ["/data"]
EXPOSE 8080
USER app
CMD ["./webserver"]
