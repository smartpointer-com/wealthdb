# Single-stage image for wealthdb: development, testing, and production
# all run in this image. See docs/DESIGN.md §12 for the rationale.
FROM golang:1.26-bookworm

WORKDIR /src

# Cache module downloads in a separate layer so source-only changes
# don't invalidate them.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# The version stamp `./wealthdb build` reads from git (internal/version).
# It changes with every commit, so the Go build cache lives in a cache
# mount: a new stamp relinks the binary instead of recompiling it.
ARG WEALTHDB_TAG=
ARG WEALTHDB_BASE=
ARG WEALTHDB_COMMIT=
RUN --mount=type=cache,target=/root/.cache/go-build \
    v=github.com/ptu-gh/wealthdb/wealthdb/internal/version && \
    go build -ldflags "-X $v.Tag=$WEALTHDB_TAG -X $v.Base=$WEALTHDB_BASE -X $v.Commit=$WEALTHDB_COMMIT" \
        -o /usr/local/bin/wealthdb ./cmd/wealthdb

# Default entrypoint is the production binary. The wealthdb-test
# wrapper overrides with --entrypoint go.
ENTRYPOINT ["/usr/local/bin/wealthdb"]
