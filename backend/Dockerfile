# Этап 1: Сборка бинарника
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o main .

# Этап 2: Минимальный образ для запуска
FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/main .

EXPOSE 9000

CMD ["./main"]