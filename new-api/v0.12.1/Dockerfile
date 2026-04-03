# 运行时镜像
FROM debian:bookworm-slim@sha256:f06537653ac770703bc45b4b113475bd402f451e85223f0f2837acbf89ab020a

ARG TARGETARCH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata libasan8 wget \
    && rm -rf /var/lib/apt/lists/* \
    && update-ca-certificates

COPY bin/new-api-${TARGETARCH} /new-api
RUN chmod +x /new-api

EXPOSE 3000
WORKDIR /data
ENTRYPOINT ["/new-api"]
