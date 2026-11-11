FROM golang:1.27.3-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa AS builder
COPY --from=busybox:1.36 /bin/busybox /busybox
