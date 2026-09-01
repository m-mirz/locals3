# Build with: docker build -t locals3 .
# Run with:   docker run -p 9000:9000 -v "$PWD/data:/data" locals3
#
# The build stage pins itself to the *builder's* platform and cross-compiles to
# the target, so a multi-arch build never runs the Go toolchain under emulation.
# The final image contains only a static binary and executes nothing at build
# time, so it needs no QEMU either.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
# No runtime dependencies, so the module files alone settle the build.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/locals3 ./cmd/locals3

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/locals3 /locals3
VOLUME /data
EXPOSE 9000
ENTRYPOINT ["/locals3"]
CMD ["--dir", "/data", "--addr", ":9000"]
