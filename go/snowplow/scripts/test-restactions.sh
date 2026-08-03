#!/bin/bash

kubectl create namespace demo-system || true

kubectl apply -f crds/templates.krateo.io_restactions.yaml
kubectl apply -f testdata/rbac.yaml
kubectl apply -f testdata/rbac.restactions.yaml
kubectl apply -f testdata/restactions
