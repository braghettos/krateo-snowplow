#!/bin/bash

kubectl create namespace demo-system || true

kubectl apply -f crds/templates.krateo.io_restactions.yaml
kubectl apply -f testdata/widgets/widgets.templates.krateo.io_buttons.yaml

kubectl apply -f testdata/devs-read-only.role.yaml

kubectl apply -f testdata/widgets/button.with.resourcesrefstemplate_ex.yaml
