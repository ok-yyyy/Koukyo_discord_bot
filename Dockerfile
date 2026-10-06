# Go Discord Bot Dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o bot ./cmd/bot

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/bot ./bot
RUN mkdir -p /app/data
CMD ["./bot"]
