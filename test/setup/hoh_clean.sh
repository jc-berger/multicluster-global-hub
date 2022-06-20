#!/bin/bash

HUB_CLUSTER_NUM=${HUB_CLUSTER_NUM:-1}
MANAGED_CLUSTER_NUM=${MANAGED_CLUSTER_NUM:-2}

CURRENT_DIR=$(cd "$(dirname "$0")" || exit;pwd)
CONFIG_DIR=${CURRENT_DIR}/config
LOG=${CONFIG_DIR}/hoh_setup.log
PID=${CONFIG_DIR}/pid

source ${CURRENT_DIR}/common.sh

checkDir ${CONFIG_DIR}
checkKind
checkKubectl
checkClusteradm

LEAF_HUB_NAME="hub1"
HUB_OF_HUB_NAME="hub-of-hubs"

CTX_HUB="kind-hub-of-hubs"
CTX_MANAGED="kind-hub1"

# kubeconfig
KUBECONFIG=${CONFIG_DIR}/kubeconfig

touch "$PID"
while read -r line; do
  if [[ $line != "" ]]; then
    kill -9 "${line}" >/dev/null 2>&1
  fi
done <"${PID}"

rm "${PID}" >/dev/null 2>&1

for i in $(seq 1 "${HUB_CLUSTER_NUM}"); do
  kind delete cluster --name "hub${i}"
  for j in $(seq 1 "${MANAGED_CLUSTER_NUM}"); do
    kind delete cluster --name "hub${i}-cluster${j}"
  done
done
kind delete cluster --name "$HUB_OF_HUB_NAME"
rm -rf "$CONFIG_DIR"

