#
# (c) Copyright IBM Corp. 2021
# (c) Copyright VC Inc.
#

# Build the manager binary, always build on amd64 platform
FROM --platform=linux/amd64 golang:1.16 as builder

ARG TARGETPLATFORM='linux/amd64'
ARG VERSION=dev
ARG GIT_COMMIT=unspecified

WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY version/ version/

# Build, injecting VERSION and GIT_COMMIT directly in the code
RUN export ARCH=$(case "${TARGETPLATFORM}" in 'linux/amd64') echo 'amd64' ;; 'linux/arm64') echo 'arm64' ;; 'linux/s390x') echo 's390x' ;; 'linux/ppc64le') echo 'ppc64le' ;; esac) \
    && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" GO111MODULE=on \
	go build -ldflags="-X 'github.com/jhonmac666/agent-operator/version.Version=${VERSION}' -X 'github.com/jhonmac666/agent-operator/version.GitCommit=${GIT_COMMIT}'" -a -o manager main.go

# Resulting image with actual Operator
FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
MAINTAINER VC, support@vc.com

ARG TARGETPLATFORM='linux/amd64'
ARG VERSION=dev
ARG BUILD=1
ARG GIT_COMMIT=unspecified
ARG DATE=

LABEL name="agent-operator" \
      vendor="VC Inc" \
      maintainer="VC Inc" \
      version=$VERSION \
      release=$VERSION \
      build=$BUILD \
      build-date=$DATE \
      git-commit=$GIT_COMMIT \
      summary="Kubernetes / OpenShift Operator for the VC APM Agent" \
      description="This operator will deploy a daemon set to run the VC APM Agent on each cluster node." \
      url="https://hub.docker.com/r/jm666/agent-operator" \
      io.k8s.display-name="VC Agent Operator" \
      io.openshift.tags="" \
      io.k8s.description="" \
      com.redhat.build-host="" \
      com.redhat.component=""

ENV OPERATOR=agent-operator \
    USER_UID=1001 \
    USER_NAME=agent-operator

WORKDIR /
COPY --from=builder /workspace/manager .
COPY LICENSE /licenses/

RUN mkdir -p .cache/helm/repository/
RUN chown -R ${USER_UID}:${USER_UID} .cache

USER ${USER_UID}:${USER_UID}
ENTRYPOINT ["/manager"]
