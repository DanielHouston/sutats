FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# Keep modules and the Go build cache in BuildKit cache mounts so a source
# change doesn't recompile every dependency from scratch.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/sutats ./cmd/sutats

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sutats /sutats
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/sutats"]
