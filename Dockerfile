##########################################
# STEP 1: build crusoe-cloud-controller-manager binary #
##########################################

FROM golang:1.23.3 AS builder

WORKDIR /build

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN make cross

################################################################
# STEP 2: build a small image and run crusoe-cloud-controller-manager binary #
################################################################

# Dockerfile.goreleaser should be kept roughly in sync
FROM alpine:3.20.3

COPY --from=builder /build/dist/crusoe-cloud-controller-manager /usr/local/go/bin/crusoe-cloud-controller-manager

ENTRYPOINT ["/usr/local/go/bin/crusoe-cloud-controller-manager"]