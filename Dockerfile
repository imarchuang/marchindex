# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod ./

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /marchindex ./cmd/marchindex

# Final stage
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata && \
    mkdir -p /data

WORKDIR /

COPY --from=builder /marchindex /marchindex

EXPOSE 9200

ENTRYPOINT ["/marchindex"]
CMD ["-dataDir", "/data", "-addr", ":9200"]
