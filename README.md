# locals3

A filesystem-backed S3 mock server for local development and tests.

`locals3` speaks enough of the S3 HTTP API for the AWS SDK to talk to it
unmodified, and stores every object as a **plain file in a local folder**. You
can `ls`, `cat`, `git`-track and hand-edit your fixtures, and a file you drop
into the folder shows up as an object.

Point your app at it instead of a real bucket while developing against Ceph/Rook
RGW, GCS's S3-compatible layer, or AWS S3.

```
data/
  photos/                  <- a bucket
    2026/cat.jpg           <- the object "2026/cat.jpg", byte for byte
  .locals3/                <- metadata, staging and in-flight uploads
```

## Run it

```bash
go build -o locals3 ./cmd/locals3
./locals3 --dir ./data --addr :9000
```

Any credentials work — signatures are parsed but never verified:

```bash
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1
aws --endpoint-url http://localhost:9000 s3 mb s3://demo
aws --endpoint-url http://localhost:9000 s3 cp ./report.pdf s3://demo/docs/report.pdf
ls -l data/demo/docs/report.pdf          # it is just a file

echo hello > data/demo/dropped.txt       # drop one in by hand
aws --endpoint-url http://localhost:9000 s3 ls s3://demo/ --recursive
```

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--dir` | `data` | Object storage root, created if absent |
| `--addr` | `:9000` | Listen address |
| `--region` | `us-east-1` | Region advertised to clients |
| `--domain` | `localhost` | Virtual-host suffix; empty for path-style only |
| `--auto-create` | `false` | Create buckets implicitly on first write |
| `--log-level` | `info` | `error`, `info` or `debug` |
| `--latency` | `0` | Delay injected into every request, e.g. `50ms` |
| `--fail-rate` | `0` | Fraction of requests answered `503 SlowDown` |

The last two exist to exercise client retry and backoff, which is otherwise hard
to test locally.

## Point a client at it

Path-style addressing is required unless you configure `--domain` and resolve
`<bucket>.<domain>` to the server.

```go
client := s3.NewFromConfig(aws.Config{
    Region:      "us-east-1",
    Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
}, func(o *s3.Options) {
    o.BaseEndpoint = aws.String("http://localhost:9000")
    o.UsePathStyle = true
})
```

Other SDKs need the same two settings: an endpoint override and path-style
addressing.

## Use it in tests

`NewTestServer` starts an in-process server on a temporary directory and tears
both down when the test ends.

```go
func TestUploadsReport(t *testing.T) {
    srv := locals3.NewTestServer(t)

    client := s3.NewFromConfig(aws.Config{
        Region:      "us-east-1",
        Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
    }, func(o *s3.Options) {
        o.BaseEndpoint = aws.String(srv.URL)
        o.UsePathStyle = true
    })

    // ... exercise the code under test ...

    // srv.Dir is the storage root, so assertions can look at the files.
    got, err := os.ReadFile(filepath.Join(srv.Dir, "reports", "2026", "q1.csv"))
}
```

locals3 has **no runtime dependencies** — it does not import the AWS SDK, so
your project keeps whatever SDK version it already uses.

## What is implemented

- Buckets: create, delete, head, list
- Objects: put, get, head, delete, bulk delete, server-side copy
- Listing: ListObjectsV2 and V1 — prefix, delimiter and `CommonPrefixes`,
  `MaxKeys`, continuation tokens, `StartAfter`/`Marker`
- Multipart upload: create, upload part, list parts, complete, abort, list
  uploads, with S3's multipart ETag (`<digest>-<partCount>`) and its 5 MiB
  minimum part size
- Range GET, `Content-Range`, 416 on unsatisfiable ranges
- Conditional requests: `If-Match`, `If-None-Match`, `If-Modified-Since`,
  `If-Unmodified-Since`
- Presigned GET and PUT, including expiry
- Checksums: `Content-MD5` and `x-amz-checksum-{crc32,crc32c,sha1,sha256}`,
  validated up front or from an `aws-chunked` trailer, with `BadDigest` and a
  rolled-back write on mismatch
- User metadata (`x-amz-meta-*`), content-type inference for foreign files
- Path-style and virtual-host addressing

Not implemented: versioning, ACLs and bucket policies, IAM, SSE, lifecycle
rules, replication, CORS configuration, website hosting, S3 Select, Batch.
Unimplemented operations answer `501 NotImplemented` naming the operation,
rather than a confusing 404.

## Behaviour worth knowing

**Signatures are never verified.** Any access key and secret are accepted, and
unsigned requests work too — `curl` can drive the server directly. Presigned URL
*expiry* is enforced, because an expired link is a failure worth reproducing.

**Keys map to paths, which constrains a few of them.** Empty path segments
(`a//b`, `/a`) and dot segments (`a/../b`) are rejected: they have no filesystem
representation. Storing both `a` and `a/b` is impossible in a mirror layout, so
the second write is refused with `409 InvalidRequest` naming the conflict rather
than corrupting the tree. Real S3 allows all of these.

**Files are authoritative, metadata is disposable.** Content type, user metadata
and cached digests live in `.locals3/meta/`. Delete that tree and every object
still reads correctly, with its type and ETag recomputed. Edit a file in place
and the next `GET` reflects the new bytes and a fresh ETag.

**Writes are atomic.** Objects are staged under `.locals3/tmp/`, fsynced, then
renamed into place, so a concurrent reader never sees a partial object.

**Timestamps have second granularity**, matching what S3 reports, so that
`If-Modified-Since` behaves consistently with the `Last-Modified` clients were
given.

## Development

```bash
go test ./...                                        # full suite
go test -race ./...                                  # concurrency and atomicity
go test -fuzz FuzzKeyPath -fuzztime 60s ./internal/store/
```

The tests drive the server through the real AWS SDK for Go v2, including
`feature/s3/manager`, on the principle that if the SDK is satisfied the mock is
right. The SDK is a test-only dependency.

## Layout

```
cmd/locals3/      the binary
server.go         Options, Server
testserver.go     NewTestServer
internal/store/   the filesystem backend; knows nothing about HTTP
internal/s3api/   the S3 wire protocol; knows nothing about the filesystem
```
