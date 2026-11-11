FROM golang:1.27.3-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    AS builder
RUN echo a \
    # a comment inside the continuation
    b
