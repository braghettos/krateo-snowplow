#!/bin/bash

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

${SCRIPT_DIR}/kind-down.sh

${SCRIPT_DIR}/kind-up.sh

kubectl create namespace demo-system || true

${SCRIPT_DIR}/build.sh
${SCRIPT_DIR}/jqmodule-to-configmap.sh

kubectl apply -f manifests/

