#!/bin/bash

kubectl create namespace demo-system || true

kubectl apply -f crds/templates.krateo.io_restactions.yaml
kubectl apply -f testdata/widgets/widgets.templates.krateo.io_tables.yaml

kubectl apply -f testdata/rbac.yaml
kubectl apply -f testdata/rbac.restactions.yaml
kubectl apply -f testdata/rbac.widgets.yaml

kubectl apply -f testdata/widgets/table.of.namespaces.yaml
