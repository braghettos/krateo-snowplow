#!/bin/bash

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

kubectl create namespace demo-system || true

kubectl delete -f manifests/deploy.snowplow.yaml

${SCRIPT_DIR}/build.sh
${SCRIPT_DIR}/jqmodule-to-configmap.sh

kubectl apply -f manifests/deploy.snowplow.yaml

