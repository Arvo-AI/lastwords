FROM debian:bookworm-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
COPY worker.c /src/worker.c
RUN gcc -g -O0 -o /thumbnail-worker /src/worker.c

FROM debian:bookworm-slim
COPY --from=build /thumbnail-worker /usr/local/bin/thumbnail-worker
CMD ["thumbnail-worker"]
