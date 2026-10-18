# Build: Go cross-kompiliert, daher kein QEMU nötig.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# timetzdata: Zeitzonen eingebettet, damit TZ=Europe/Berlin im scratch-Image wirkt
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags timetzdata -ldflags "-s -w -X main.version=${VERSION}" -o /out/mikrotik-manager . \
 && mkdir -p /out/data

# Laufzeit: nur das Binary + CA-Zertifikate.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/mikrotik-manager /mikrotik-manager
COPY --from=build --chown=65534:65534 /out/data /data
ENV DATA_DIR=/data \
    LISTEN=:8080 \
    MM_DOCKER=1 \
    GOMEMLIMIT=48MiB
USER 65534:65534
EXPOSE 8080/tcp 5678/udp
VOLUME /data
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/mikrotik-manager", "healthcheck"]
ENTRYPOINT ["/mikrotik-manager"]
