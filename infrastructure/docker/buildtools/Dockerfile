# syntax=docker/dockerfile:1.7
# Custom base image: pre-baked apk toolchain for media-plane mediasoup native compile.
# Published as ghcr.io/concord-voice/node-buildtools:<FROM-tag>-<YYYY-MM-DD>.
# Refresh discipline: docs/runbooks/refresh-buildtools-base.md.

FROM node:26-alpine@sha256:c6ad9e9c1d80f6eb607c26b4b0a132ff7a2344fad1c78ea3baa2a7d4c8de6d94

RUN apk add --no-cache python3 py3-pip make g++ linux-headers

LABEL org.opencontainers.image.source="https://github.com/Concord-Voice/Concord-Voice"
LABEL org.opencontainers.image.description="Pre-baked apk toolchain for services/media-plane/Dockerfile builder stage"
LABEL org.opencontainers.image.documentation="https://github.com/Concord-Voice/Self-Hosted/blob/main/docs/runbooks/refresh-buildtools-base.md"
LABEL org.opencontainers.image.licenses="LicenseRef-CVSL-1.0"
