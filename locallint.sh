#!/bin/sh
set -e

go build -i github.com/TrilliumIT/docker-drouter
$GOPATH/bin/gometalinter --vendor --deadline 120s --skip=$(dirname $0)/vendor $(dirname $0) $(dirname $0)/...


### to install:
# go get -u github.com/alecthomas/gometalinter
# $GOPATH/bin/gometalinter --install --update
