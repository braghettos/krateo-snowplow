#!/bin/bash

TOOLS_DIR="${GOBIN:-$(go env GOPATH)/bin}"

${TOOLS_DIR}/swag init --parseDependency -g main.go
