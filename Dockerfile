FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tg-bot ./cmd/bot

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Asia/Bangkok
WORKDIR /app
COPY --from=builder /out/tg-bot /app/tg-bot
EXPOSE 8080
ENTRYPOINT ["/app/tg-bot"]
