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

# Ciclo per creare 20 pod dummy
for i in {0..19}; do
  POD_NAME="dummy-pod-$i"

  echo "Creating pod: $POD_NAME"

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: $POD_NAME
  namespace: $NAMESPACE
spec:
  containers:
  - name: dummy
    image: busybox
    command: ["sleep", "3600"]
    resources:
      limits:
        memory: "16Mi"
        cpu: "10m"
      requests:
        memory: "8Mi"
        cpu: "5m"
  restartPolicy: Never
EOF

done

echo "✅ Tutti i pod dummy sono stati creati nel namespace '$NAMESPACE'"
