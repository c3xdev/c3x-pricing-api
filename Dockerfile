FROM golang:1.25.11-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /c3x-pricing-api ./cmd/server

FROM alpine:3.21
RUN apk --no-cache add ca-certificates && \
    addgroup -S appgroup && adduser -S -u 1000 appuser -G appgroup
COPY --from=builder /c3x-pricing-api /usr/local/bin/c3x-pricing-api
USER 1000
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:4000/healthz || exit 1
ENTRYPOINT ["c3x-pricing-api"]
CMD ["serve"]
