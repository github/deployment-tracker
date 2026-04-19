FROM golang:1.26.2-alpine3.23@sha256:c2a1f7b2095d046ae14b286b18413a05bb82c9bca9b25fe7ff5efef0f0826166 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o deployment-tracker cmd/deployment-tracker/main.go

FROM alpine:3.23@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/deployment-tracker /deployment-tracker
ENTRYPOINT ["/deployment-tracker"]
