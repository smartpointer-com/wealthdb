# Single-stage image for wealthdb: development, testing, and production
# all run in this image. See docs/DESIGN.md §12 for the rationale.
FROM golang:1.26-bookworm

WORKDIR /src

# Cache module downloads in a separate layer so source-only changes
# don't invalidate them.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN go build -o /usr/local/bin/wealthdb ./cmd/wealthdb

# Default entrypoint is the production binary. The wealthdb-test
# wrapper overrides with --entrypoint go.
ENTRYPOINT ["/usr/local/bin/wealthdb"]
