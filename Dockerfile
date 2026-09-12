# Pinned to the *build* platform so the arm64 image is cross-compiled natively
# instead of compiled inside an emulated arm64 VM, which is many times slower.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

ARG TARGETOS TARGETARCH
COPY *.go ui.html ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/sky-notify .

# /data and /config are created here so they can be copied in with the runtime uid. Docker
# seeds a fresh named volume from the image path *including its ownership* — without this
# the non-root process cannot create db.json or state.json on a brand-new volume, and the
# web UI cannot write alerts.yaml back to its default location.
RUN mkdir -p /out/data /out/config

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sky-notify /sky-notify
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/config /config

VOLUME /data
EXPOSE 8080
USER 65532:65532

# The image has no shell, curl or wget, so the binary probes itself.
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/sky-notify", "-healthcheck"]

ENTRYPOINT ["/sky-notify"]
