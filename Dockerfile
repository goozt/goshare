FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o goshare .

FROM alpine:latest
RUN adduser -D -u 1000 appuser
WORKDIR /app
COPY --from=builder /app/goshare .
RUN mkdir -p /data && chown appuser:appuser /data
USER appuser
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["./goshare", "-d", "/data", "-p", "8080"]
