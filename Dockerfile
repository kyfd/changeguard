FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dbguard ./cmd/dbguard

FROM alpine:3.20
RUN apk add --no-cache ca-certificates postgresql-client tzdata && addgroup -S dbguard && adduser -S dbguard -G dbguard
WORKDIR /app
COPY --from=builder /out/dbguard /app/dbguard
RUN mkdir -p /app/data && chown -R dbguard:dbguard /app
USER dbguard
ENV PORT=8080 DBGUARD_DATA_FILE=/app/data/dbguard.json TZ=Asia/Shanghai
EXPOSE 8080
VOLUME ["/app/data"]
ENTRYPOINT ["/app/dbguard"]
