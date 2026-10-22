# Multi-stage build producing the three service images and the operator CLI
# from one module (SDD 12.2). Each service runs its own binary rather than one
# binary with a mode flag: the public API image has no admin routes compiled
# into it at all, which is the same isolation argument as the separate signing
# keys (SDD 10.2).
#
# Build everything with one image and pick the binary per invocation:
#   docker build -t matchup .
#   docker run matchup /app/api        (or /app/admin, /app/worker)
#   docker run -it --env-file .env matchup /app/cli createsuperuser

# The builder is pinned to the toolchain in go.mod and documented in SDD 12.2.
FROM golang:1.26-alpine AS builder

# git is needed for module downloads over VCS; ca-certificates for the proxy.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. This layer is invalidated only by go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and a static link, because the runtime image has no libc.
# -trimpath keeps build paths out of the binary; the ldflags drop the symbol
# table and DWARF, which is most of the size.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/api    ./cmd/api    \
 && go build -trimpath -ldflags="-s -w" -o /out/admin  ./cmd/admin  \
 && go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
 && go build -trimpath -ldflags="-s -w" -o /out/cli    ./cmd/cli

# The migration tool ships in the image so a deploy can apply schema changes
# with the exact version that was tested against them, and so a release step
# needs nothing but this image. Pinned deliberately: an unpinned @latest would
# make the schema of a rebuilt old release depend on the day it was rebuilt.
RUN go install github.com/pressly/goose/v3/cmd/goose@v3.26.0

# distroless/static: no shell, no package manager, no libc. There is nothing to
# exec if someone finds a way to run a command, and nonroot means the process
# cannot write to its own filesystem.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/api    /app/api
COPY --from=builder /out/admin  /app/admin
COPY --from=builder /out/worker /app/worker
COPY --from=builder /out/cli    /app/cli
COPY --from=builder /go/bin/goose /app/goose
# Carried so the migration step is `goose up` against a path in the image
# rather than a volume mount that has to match the source tree.
COPY migrations /app/migrations

USER nonroot:nonroot

# Documentation only; the ports come from PORT and ADMIN_PORT.
EXPOSE 8080 8081

# No default: the three binaries are not interchangeable and picking one here
# would make a compose file or deploy config that forgot its command start the
# wrong process quietly.
ENTRYPOINT []
