FROM ubuntu:14.04
MAINTAINER jdoliner@pachyderm.io

# Install FUSE
RUN \
  apt-get update -yq && \
  apt-get install -yq --no-install-recommends \
    git \
    ca-certificates \
    curl \
    fuse && \
  apt-get clean && \
  rm -rf /var/lib/apt

# Install Go 1.5.2
RUN curl -s https://storage.googleapis.com/golang/go1.5.2.linux-amd64.tar.gz | tar -v -C /usr/local/ -xz
ENV PATH /usr/local/go/bin:/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin
ENV GOPATH /go
ENV GOROOT /usr/local/go

# Install Pachyderm job-shim
RUN go get github.com/pachyderm/pachyderm/src/cmd/job-shim && \
    cp $GOPATH/bin/job-shim /job-shim
