# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/podcastdelay ./cmd/podcastdelay

# CA certs for HTTPS fetches of source feeds; scratch/distroless has none.
RUN cp /etc/ssl/certs/ca-certificates.crt /out/ca-certificates.crt

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/podcastdelay /podcastdelay

ENV PODCASTDELAY_DATA_DIR=/data
ENV PODCASTDELAY_ADDR=:8080

VOLUME ["/data"]
EXPOSE 8080

USER nonroot:nonroot

HEALTHCHECK --interval=60s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/podcastdelay", "healthcheck"]

ENTRYPOINT ["/podcastdelay"]
CMD ["serve"]
