#!/bin/bash

# Nome del namespace
NAMESPACE="demo-system"

# Crea il namespace se non esiste
kubectl get namespace $NAMESPACE >/dev/null 2>&1
if [ $? -ne 0 ]; then
  echo "Creating namespace: $NAMESPACE"
  kubectl create namespace $NAMESPACE
else
  echo "Namespace $NAMESPACE already exists"
fi

kubectl apply -f crds/templates.krateo.io_restactions.yaml
kubectl apply -f testdata/pagination/widget.page.schema.crd.yaml
kubectl apply -f testdata/widgets/widgets.templates.krateo.io_buttons.yaml


kubectl apply -f testdata/pagination/restaction-list-pods.yaml
kubectl apply -f testdata/pagination/widget.page.sample.yaml
kubectl apply -f testdata/pagination/widget.yaml

kubectl apply -f testdata/rbac.pods.yaml
kubectl apply -f testdata/rbac.restactions.yaml
kubectl apply -f testdata/rbac.widgets.yaml
