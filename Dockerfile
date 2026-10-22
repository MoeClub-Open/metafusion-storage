FROM golang:1.27.3-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/storage-server ./cmd/server

FROM alpine:3.24.3
WORKDIR /app
COPY --from=builder /app/storage-server /app/storage-server
EXPOSE 8082
CMD ["/app/storage-server"]
