FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY *.go ./
COPY quant/ ./quant/
COPY alor/ ./alor/
COPY agent/ ./agent/
COPY secure/ ./secure/
COPY optioncalc/ ./optioncalc/
COPY static/ ./static/
RUN CGO_ENABLED=0 GOOS=linux go build -o main .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/main .
COPY --from=builder /app/static ./static

EXPOSE 9000
CMD ["./main"]
