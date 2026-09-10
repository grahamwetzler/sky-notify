FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sky-notify .

# /data is created here so it can be copied in with the runtime uid. Docker seeds a
# fresh named volume from the image path *including its ownership* — without this the
# non-root process cannot create db.json or state.json on a brand-new volume.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sky-notify /sky-notify
COPY --from=build --chown=65532:65532 /out/data /data

VOLUME /data
EXPOSE 8080
USER 65532:65532

# The image has no shell, curl or wget, so the binary probes itself.
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/sky-notify", "-healthcheck"]

ENTRYPOINT ["/sky-notify"]
