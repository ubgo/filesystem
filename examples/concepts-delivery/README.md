# Delivery: CDN-signed links, proxy offload and data URIs

Most downloads should not pass through the application at all. This program signs a CloudFront URL and a set of CloudFront signed cookies with `contrib/cloudfront`, signs a Cloudflare token-authentication URL with `contrib/cloudflare`, serves a local disk through `fshttp.Public` with `Sendfile` so nginx sends the bytes (`X-Accel-Redirect`), and inlines a tiny image with `Disk.DataURI`. It also shows what each refuses: an HTML link a CDN could not force to download, a Cloudflare link outliving the rule's TTL, a traversal path, an SVG and a file too large to inline. The guide [Serving files](../../docs/concepts/serving-files.md#step-11--signed-cdn-links) explains each step.

## Run it

```sh
cd examples
go run ./concepts-delivery
go test ./concepts-delivery
```

It needs no environment variables and no network: the CloudFront key is generated for the run, the Cloudflare secret is fixed, the CDN hosts are only used to build URLs, and the sendfile step uses a temporary directory it removes before exiting. Against a real distribution, load the key with `sign.LoadPEMPrivKey` and use the key pair ID CloudFront shows for its public key; against a real zone, put the same secret and TTL in the WAF rule.

## What it does

```text
cloudfront URL: https://cdn.example.com/media/videos/42/poster.jpg?Expires&Key-Pair-Id&Signature
cloudfront HTML link: unsupported
cloudfront cookies: CloudFront-Policy,CloudFront-Signature,CloudFront-Key-Pair-Id (Secure true, HttpOnly true)
printed signer: cloudfront.Signer{cloudfront.Config{BaseURL: "https://cdn.example.com/media", KeyPairID: "K2JCJMDEHXQW5F", PrivateKey: [REDACTED]}}
cloudflare URL: https://downloads.example.com/files/reports/q3.pdf?verify, valid 10m0s
cloudflare link longer than the rule's TTL: invalid_argument
sendfile /logos/acme%20corp.png: 200 X-Accel-Redirect="/protected/logos/acme%20corp.png" body 0 bytes | image/png | nosniff
sendfile /logos/../../etc/passwd: 400 X-Accel-Redirect="" body 12 bytes | text/plain; charset=utf-8 | nosniff
data URI pixel.png: data:image/png;base64,… (114 chars)
data URI logo.svg: rejected
data URI hero.png: too_large
```

Signed URLs print with their parameter values hidden, so the output is the same on every run.
