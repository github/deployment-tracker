 # 1.25.5-alpine3.23
FROM golang@sha256:26111811bc967321e7b6f852e914d14bede324cd1accb7f81811929a6a57fea9 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o deployment-tracker cmd/deployment-tracker/main.go

# v3.23
FROM alpine@sha256:865b95f46d98cf867a156fe4a135ad3fe50d2056aa3f25ed31662dff6da4eb62
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/deployment-tracker /deployment-tracker
ENTRYPOINT ["/deployment-tracker"]
