FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-s -w" \
    -o /sidecar \
    ./cmd/sidecar

# -----------------------------------------------------------
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /sidecar /sidecar

ENTRYPOINT ["/sidecar"]