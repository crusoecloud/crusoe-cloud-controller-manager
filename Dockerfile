##########################################
# STEP 1: build crusoe-cloud-controller-manager binary #
##########################################

FROM golang:1.26 AS builder

ARG CRUSOE_CLOUD_CONTROLLER_MANAGER_NAME
ENV CRUSOE_CLOUD_CONTROLLER_MANAGER_NAME=$CRUSOE_CLOUD_CONTROLLER_MANAGER_NAME
ARG CRUSOE_CLOUD_CONTROLLER_MANAGER_VERSION
ENV CRUSOE_CLOUD_CONTROLLER_MANAGER_VERSION=$CRUSOE_CLOUD_CONTROLLER_MANAGER_VERSION

# Private module auth: gitlab.com/crusoeenergy/schemas/* requires the CI job
# token (the CI build_and_push job already passes both build args).
ARG CI_SERVER_HOST=gitlab.com
ARG CI_JOB_TOKEN
ENV GOPRIVATE=gitlab.com/crusoeenergy/*

WORKDIR /build

COPY go.mod .
COPY go.sum .

RUN printf 'machine %s login gitlab-ci-token password %s\n' "$CI_SERVER_HOST" "$CI_JOB_TOKEN" > ~/.netrc \
	&& go mod download \
	&& rm ~/.netrc

COPY . .

RUN make cross

################################################################
# STEP 2: build a small image and run crusoe-cloud-controller-manager binary #
################################################################

# Dockerfile.goreleaser should be kept roughly in sync
FROM alpine:3.20.3

COPY --from=builder /build/dist/crusoe-cloud-controller-manager /usr/local/go/bin/crusoe-cloud-controller-manager

ENTRYPOINT ["/usr/local/go/bin/crusoe-cloud-controller-manager"]