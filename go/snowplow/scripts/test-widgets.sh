#!/bin/bash

kubectl create namespace demo-system || true

kubectl apply -f crds/templates.krateo.io_restactions.yaml
kubectl apply -f testdata/widgets/widgets.templates.krateo.io_buttons.yaml

kubectl apply -f testdata/rbac.widgets.yaml
kubectl apply -f testdata/rbac.restactions.yaml
kubectl apply -f testdata/rbac.pods.yaml
kubectl apply -f testdata/rbac.namespaces.yaml

kubectl apply -f testdata/widgets/button.restaction.simple.yaml
kubectl apply -f testdata/widgets/button.sample.yaml
kubectl apply -f testdata/widgets/button.with.api.yaml
kubectl apply -f testdata/widgets/button.with.api.error.yaml
kubectl apply -f testdata/widgets/button.with.api.and.resourcesrefs.yaml
kubectl apply -f testdata/widgets/button.with.resourcesrefs.yaml
kubectl apply -f testdata/widgets/button.with.resourcesrefstemplate.yaml