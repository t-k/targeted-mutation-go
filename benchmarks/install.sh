#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "Usage: benchmarks/install.sh OUTPUT_DIRECTORY" >&2
    exit 1
fi

mkdir -p "$1"
GOBIN=$(cd "$1" && pwd)
export GOBIN
go build -o "$GOBIN/targeted-mutation-go" ./cmd/targeted-mutation-go
go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0
go install github.com/gomutants/gomutants@v0.7.3
go install github.com/sivchari/gomu/cmd/gomu@v0.3.0
go install github.com/P4suta/go-mutants/cmd/go-mutants@v0.1.2
