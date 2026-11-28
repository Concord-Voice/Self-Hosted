# syntax=docker/dockerfile:1.7
# Custom base image: pre-baked apk toolchain for media-plane mediasoup native compile.
# Published as ghcr.io/concord-voice/node-buildtools:<FROM-tag>-<YYYY-MM-DD>.
# Refresh discipline: docs/runbooks/refresh-buildtools-base.md.

FROM node:26-alpine@sha256:6bd9ab43c0ec0190ca7b2d193906cb8b7edf6e7f4afa43ddeccd6452710bffb5

RUN apk add --no-cache python3 py3-pip make g++ linux-headers

LABEL org.opencontainers.image.source="https://github.com/Concord-Voice/Concord-Voice"
LABEL org.opencontainers.image.description="Pre-baked apk toolchain for services/media-plane/Dockerfile builder stage"
LABEL org.opencontainers.image.documentation="https://github.com/Concord-Voice/Self-Hosted/blob/main/docs/runbooks/refresh-buildtools-base.md"
LABEL org.opencontainers.image.licenses="LicenseRef-CVSL-1.0"
