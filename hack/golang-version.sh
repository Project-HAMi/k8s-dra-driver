#!/bin/bash

SCRIPTS_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )"/../hack && pwd )"

GOLANG_VERSION=$(grep -E "^go .*$" go.mod | grep -oE "[0-9\.]+")

echo $GOLANG_VERSION
