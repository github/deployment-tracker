 # 1.25.7-alpine3.23
FROM golang@sha256:9edf71320ef8a791c4c33ec79f90496d641f306a91fb112d3d060d5c1cee4e20 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o deployment-tracker cmd/deployment-tracker/main.go

# v3.23
FROM alpine@sha256:51183f2cfa6320055da30872f211093f9ff1d3cf06f39a0bdb212314c5dc7375
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/deployment-tracker /deployment-tracker
ENTRYPOINT ["/deployment-tracker"]
