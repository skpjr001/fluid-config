FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fluid-config .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /fluid-config /fluid-config
WORKDIR /app
USER nonroot
EXPOSE 8090
ENTRYPOINT ["/fluid-config", "-config", "/app/config.json"]
