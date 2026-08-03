#!/bin/bash

kubectl create configmap jq-custom-modules \
  --from-file=custom.jq=testdata/custom-modules.jq \
  --namespace=demo-system
