FROM golang:1.22-alpine AS builder

WORKDIR /workspace
COPY go.mod ./
COPY *.go ./
COPY quant/ ./quant/
COPY alor/ ./alor/
COPY agent/ ./agent/
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/main .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/main .
COPY static/ ./static/

EXPOSE 9000
CMD ["./main"]
