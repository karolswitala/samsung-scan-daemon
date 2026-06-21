FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /samsung-scan ./cmd/samsung-scan
RUN mkdir /scans && chown 65534:65534 /scans

FROM scratch
COPY --from=builder /samsung-scan /samsung-scan
COPY --from=builder /scans /scans
USER 65534:65534
ENTRYPOINT ["/samsung-scan"]
