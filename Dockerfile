FROM docker.io/library/golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY ui ./ui
COPY schema ./schema
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fluid-config . \
 && mkdir -p /out/app/data /out/app/dynamic/apis

FROM gcr.io/distroless/static:nonroot
COPY --from=build /fluid-config /fluid-config
COPY --from=build --chown=65532:65532 /out/app /app
WORKDIR /app
USER nonroot
EXPOSE 8090 9090
VOLUME ["/app/data", "/app/dynamic"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s CMD ["/fluid-config", "healthcheck", "-config", "/app/config.json"]
ENTRYPOINT ["/fluid-config", "-config", "/app/config.json"]
