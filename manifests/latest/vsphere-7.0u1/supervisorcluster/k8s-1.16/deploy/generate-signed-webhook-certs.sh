#!/bin/bash
# File originally from https://github.com/istio/istio/blob/release-0.7/install/kubernetes/webhook-create-signed-cert.sh
set -e

if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
    cat <<EOF
Usage: Generate certificate suitable for use with an webhook service.
This script uses k8s' CertificateSigningRequest API to a generate a
certificate signed by k8s CA suitable for use with sidecar-injector webhook
services. This requires permissions to create and approve CSR. See
https://kubernetes.io/docs/tasks/tls/managing-tls-in-a-cluster for
detailed explanation and additional instructions.
usage: ${0} [OPTIONS]
The following flags are required.
       --service          Service name of webhook.
       --namespace        Namespace where webhook service and secret reside.
EOF
    exit 1
fi

while [[ $# -gt 0 ]]; do
    case ${1} in
        --service)
            service="$2"
            shift
            ;;
        --namespace)
            namespace="$2"
            shift
            ;;
        *)
            usage
            ;;
    esac
    shift
done

[ -z "${service}" ] && service=sc-validation-webhook-svc
[ -z "${namespace}" ] && namespace=vmware-system-csi


if [ ! -x "$(command -v openssl)" ]; then
    echo "openssl not found"
    exit 1
fi

if [ ! -x "$(command -v kubectl)" ]; then
    echo "kubectl not found"
    exit 1
fi

csrName=${service}.${namespace}
certdir=$(dirname "$0")/certs
mkdir -p "${certdir}"

echo "creating certs in dir ${certdir} "

cat <<EOF >> "${certdir}"/csr.conf
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${service}
DNS.2 = ${service}.${namespace}
DNS.3 = ${service}.${namespace}.svc
EOF

openssl genrsa -out "${certdir}"/server-key.pem 2048
openssl req -new -key "${certdir}"/server-key.pem -subj "/O=system:nodes/CN=system:node:127.0.0.1" -out "${certdir}"/server.csr -config "${certdir}"/csr.conf


# clean-up any previously created CSR for our service. Ignore errors if not present.
kubectl delete csr ${csrName} 2>/dev/null || true

# create  server cert/key CSR and  send to k8s API
cat <<EOF | kubectl create -f -
apiVersion: certificates.k8s.io/v1beta1
kind: CertificateSigningRequest
metadata:
  name: ${csrName}
spec:
  request: $(base64 "${certdir}"/server.csr | tr -d '\n')
  signerName: kubernetes.io/kubelet-serving
  usages:
  - digital signature
  - key encipherment
  - server auth
EOF

# verify CSR has been created
while true; do
    if kubectl get csr "${csrName}"; then
        break
    fi
done

# approve and fetch the signed certificate
kubectl certificate approve ${csrName}
# verify certificate has been signed
for _ in $(seq 10); do
    serverCert=$(kubectl get csr ${csrName} -o jsonpath='{.status.certificate}')
    if [[ ${serverCert} != '' ]]; then
        break
    fi
    sleep 1
done
if [[ ${serverCert} == '' ]]; then
    echo "ERROR: After approving csr ${csrName}, the signed certificate did not appear on the resource. Giving up after 10 attempts." >&2
    exit 1
fi
echo "${serverCert}" | openssl base64 -d -A -out "${certdir}"/server-cert.pem

echo "add admission-webhook-cert in the vsphere-config-secret with certificate from ${certdir}/server-cert.pem"
echo "add admission-webhook-key in the vsphere-config-secret with private key from ${certdir}/server-key.pem"

rm -rf "${certdir}"/server.csr
rm -rf "${certdir}"/csr.conf 