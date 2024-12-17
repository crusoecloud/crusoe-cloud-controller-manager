##########################################
# STEP 1: build crusoe-cloud-controller-manager binary #
##########################################

FROM golang:1.22.9 AS builder

ARG CRUSOE_CLOUD_CONTROLLER_MANAGER_NAME
ENV CRUSOE_CLOUD_CONTROLLER_MANAGER_NAME=$CRUSOE_CLOUD_CONTROLLER_MANAGER_NAME
ARG CRUSOE_CLOUD_CONTROLLER_MANAGER_VERSION
ENV CRUSOE_CLOUD_CONTROLLER_MANAGER_VERSION=$CRUSOE_CLOUD_CONTROLLER_MANAGER_VERSION

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