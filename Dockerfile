FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /samsung-scan ./cmd/samsung-scan

FROM scratch
COPY --from=builder /samsung-scan /samsung-scan
ENTRYPOINT ["/samsung-scan"]
