# SPDX-License-Identifier: AGPL-3.0-only
# One image with every farm program and the skeleton checkout the core reads at run time.
# Build context: the repository root (compose.yaml sets it).
FROM golang:1.27-bookworm AS build
WORKDIR /src/impl
COPY impl/go.mod impl/go.sum ./
RUN go mod download
COPY impl/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/...

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --home-dir /nonexistent --shell /usr/sbin/nologin botaru \
    && install -d -o 10001 -m 700 /var/lib/botaru/workers /var/lib/botaru/workers/agent /var/lib/botaru/workers/channel
COPY --from=build /out/ /opt/botaru/bin/
# The skeleton: schemas, bindings, texts and the examples the core and the channel read.
COPY spec/ /opt/botaru/src/spec/
COPY examples/ /opt/botaru/src/examples/
ENV PATH=/opt/botaru/bin:$PATH BOTARU_SPEC_ROOT=/opt/botaru/src
USER 10001
WORKDIR /opt/botaru/src
