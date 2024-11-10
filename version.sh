#!/bin/bash

VERSION=$(git describe --tags --always || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")
GO_VERSION=$(go version | cut -d' ' -f3)

echo "$VERSION $COMMIT built with $GO_VERSION"