#!/bin/sh
set -e

$GOPATH/bin/gometalinter --disable=gocyclo --vendor --deadline 120s --skip=$(dirname $0)/vendor $(dirname $0) $(dirname $0)/...


### to install:
# go get -u github.com/alecthomas/gometalinter
# $GOPATH/bin/gometalinter --install --update
