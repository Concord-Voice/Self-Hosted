FROM golang:1.27.3-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa AS builder
RUN --mount=type=bind,from=busybox:1.36,target=/x true
