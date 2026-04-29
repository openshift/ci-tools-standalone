#!/usr/bin/env bash

set -euxo pipefail

echo "Running golangci-lint"
if [[ $HOME = '/' ]]; then
  export HOME=/tmp
fi

if [[ -n ${CI:-} ]];
then
  golangci-lint run
else
  DOCKER=${DOCKER:-podman}

  if ! which "$DOCKER" > /dev/null 2>&1;
  then
    echo "$DOCKER not found, please install."
    exit 1
  fi

  $DOCKER run --rm \
    --volume "${PWD}:/go/src/github.com/openshift/ci-tools-standalone:z" \
    --workdir /go/src/github.com/openshift/ci-tools-standalone \
    quay-proxy.ci.openshift.org/openshift/ci:ci_golangci-lint_latest \
    golangci-lint run
fi
