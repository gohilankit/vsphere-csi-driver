#!/bin/bash
set -e

if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
  cat <<EOF
Usage: Patch validatingwebhook.yaml with CA_BUNDLE retrieved from Kubernetes API server
and create ValidatingWebhookConfiguration and sc-validation-webhook-svc service using patched yaml file
EOF
  exit 1
fi

CA_BUNDLE=$(kubectl get configmap -n kube-system extension-apiserver-authentication -o=jsonpath='{.data.client-ca-file}' | base64 | tr -d '\n')

# clean-up previously created service and validatingwebhookconfiguration. Ignore errors if not present.
kubectl delete validatingwebhookconfiguration.admissionregistration.k8s.io validation.csi.vsphere.vmware.com 2>/dev/null || true

kubectl delete service sc-validation-webhook-svc --namespace vmware-system-csi 2>/dev/null || true

# patch validatingwebhook.yaml with CA_BUNDLE and create service and validatingwebhookconfiguration
sed "s/caBundle: .*$/caBundle: ${CA_BUNDLE}/g" validatingwebhook.yaml | kubectl apply -f -