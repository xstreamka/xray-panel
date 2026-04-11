FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /xray-panel ./cmd/server

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /xray-panel .
COPY internal/templates ./internal/templates
COPY static ./static

ENV TZ=Europe/Moscow

CMD ["./xray-panel"]
