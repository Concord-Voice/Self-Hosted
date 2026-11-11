FROM golang:1.27.3-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa AS builder
FROM scratch
COPY --from=builder /a /a
RUN --mount=type=bind,from=builder,target=/x true
