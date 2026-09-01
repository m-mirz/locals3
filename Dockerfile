# Build with: docker build -t locals3 .
# Run with:   docker run -p 9000:9000 -v "$PWD/data:/data" locals3
FROM golang:1.25-alpine AS build
WORKDIR /src
# No runtime dependencies, so the module files alone settle the build.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/locals3 ./cmd/locals3

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/locals3 /locals3
VOLUME /data
EXPOSE 9000
ENTRYPOINT ["/locals3"]
CMD ["--dir", "/data", "--addr", ":9000"]
