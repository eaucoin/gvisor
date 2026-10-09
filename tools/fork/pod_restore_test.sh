#!/bin/bash

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# pod_restore_test.sh checks runsc and its containerd shim in Kubernetes: a
# one-node k3s cluster, in a Docker container, runs a pod that counts in its
# memory under runsc; the pod's sandbox is checkpointed through containerd's
# task service (the shim's Checkpoint), once leaving it running and once
# stopping it, and restored into new pods by the restore annotations
# (g3doc/user_guide/checkpoint_restore.md), in the background:
#
#   - a clone of the running pod, and the stopped pod in a new pod, must count
#     on from their checkpointed state;
#   - so must the stopped pod restored with another memory limit (the kubelet
#     derives the process's OOM score adjustment from it);
#   - a pod whose checkpoint does not exist must fail, not start afresh.
#
# Then a plain containerd client, with no CRI annotations: a container
# checkpointed into containerd's content store ("ctr containers checkpoint")
# and restored from it ("ctr containers restore --live", which creates its task
# with CreateTaskRequest.checkpoint) must count on likewise.
#
# It needs root (through sudo) and Docker.
#
# Usage: tools/fork/pod_restore_test.sh DIR [RUNSC_FLAG...]
#
# DIR holds runsc, its gvisor-bin/ and containerd-shim-runsc-v1, as a release
# tarball unpacks; each RUNSC_FLAG, --name=value, is set for every sandbox.

set -euo pipefail

declare dir
dir="$(realpath "${1:?usage: pod_restore_test.sh DIR [RUNSC_FLAG...]}")"
readonly DIR="${dir}"
shift

# k3s v1.37.1+k3s1, with containerd v2.3.4, and busybox, Docker Hub's images
# from Google's mirror of Docker Hub: Docker Hub limits the pulls of CI
# runners' shared addresses.
readonly K3S="mirror.gcr.io/rancher/k3s:v1.37.1-k3s1@sha256:ca7f37d993d82ef0dcdcfecb2e0e2618ea541dbaffc620c8cedebe01a82acd0d"
readonly IMAGE="mirror.gcr.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
readonly NODE="pod-restore-$$"
readonly NS="pod-restore"
# The workload: 48 MiB of random data in memory (/dev/shm), whose digest is its
# start token, and a count, written to /count every 0.2 s.
# shellcheck disable=SC2016 # The container's shell expands it.
readonly WORKLOAD='head -c 50331648 /dev/urandom >/dev/shm/data; start=$(md5sum </dev/shm/data | cut -c 1-16); i=0; while :; do i=$((i+1)); echo "$start $i" >/count; sleep 0.2; done'
declare conf
conf="$(dirname "$(realpath "$0")")/pod_restore"
readonly CONF="${conf}"

declare tmp
tmp="$(mktemp -d)"
readonly tmp

for f in runsc gvisor-bin containerd-shim-runsc-v1; do
  if [[ ! -e "${DIR}/${f}" ]]; then
    echo "${DIR}/${f} does not exist" >&2
    exit 1
  fi
done

# The shim's options for every container: runsc from DIR, restores in the
# background, and the given runsc flags.
{
  echo 'binary_name = "/opt/gvisor/runsc"'
  echo 'restore_background = true'
  echo '[runsc_config]'
  echo '  debug = "true"'
  echo '  debug-log = "/var/log/runsc/%ID%/"'
  for flag in "$@"; do
    if [[ ! "${flag}" =~ ^--([a-z0-9-]+)=(.*)$ ]]; then
      echo "${flag}: a runsc flag must read --name=value" >&2
      exit 1
    fi
    echo "  ${BASH_REMATCH[1]} = \"${BASH_REMATCH[2]}\""
  done
} >"${tmp}/runsc.toml"

on_node() {
  sudo docker exec -i "${NODE}" "$@"
}

cleanup() {
  local status=$?
  if (( status != 0 )); then
    # What the node and the sandboxes said, for whoever reads the failure.
    on_node kubectl -n "${NS}" get events --sort-by=.lastTimestamp >&2 || true
    on_node sh -c 'find /var/log/runsc -name "*.boot.txt" -exec tail -n 30 {} +' >&2 || true
    sudo docker logs --tail 30 "${NODE}" >&2 || true
  fi
  sudo docker rm -f "${NODE}" >/dev/null 2>&1 || true
  rm -rf "${tmp}"
}
trap cleanup EXIT

echo "Starting a k3s node with runsc from ${DIR}."
sudo docker run --detach --name "${NODE}" --hostname "${NODE}" --privileged \
  --memory 1536m --memory-swap 1536m --tmpfs /run --tmpfs /var/run \
  --volume "${CONF}/entrypoint.sh:/usr/local/bin/entrypoint.sh:ro" \
  --volume "${CONF}/config-v3.toml.tmpl:/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl:ro" \
  --volume "${CONF}/config.toml:/etc/containerd/runsc/config.toml:ro" \
  --volume "${tmp}/runsc.toml:/etc/containerd/runsc/runsc.toml:ro" \
  --volume "${DIR}:/opt/gvisor:ro" \
  --volume "${DIR}/containerd-shim-runsc-v1:/bin/containerd-shim-runsc-v1:ro" \
  --entrypoint /usr/local/bin/entrypoint.sh "${K3S}" \
  server --disable=traefik,servicelb,metrics-server,local-storage --disable-helm-controller \
  --node-name="${NODE}" >/dev/null

for (( i = 0; ; i++ )); do
  if on_node kubectl get node "${NODE}" --no-headers 2>/dev/null | grep -qw Ready; then
    break
  fi
  if (( i == 180 )); then
    echo "the node is not ready after 6 minutes" >&2
    exit 1
  fi
  sleep 2
done
on_node kubectl apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
---
apiVersion: v1
kind: Namespace
metadata:
  name: ${NS}
EOF
until on_node kubectl -n "${NS}" get serviceaccount default >/dev/null 2>&1; do
  sleep 0.5
done
echo "Node ready: $(on_node k3s --version | head -n 1); $(on_node /opt/gvisor/runsc --version | head -n 1)."

# pod NAME MEMORY [CHECKPOINT] applies a pod that runs the workload, restored
# from the checkpoint at CHECKPOINT on the node if given. A restored pod repeats the
# spec of the pod it was checkpointed from (runsc validates it), but for the
# memory limit.
pod() {
  local -r name="$1" memory="$2" checkpoint="${3:-}"
  local annotations=""
  if [[ -n "${checkpoint}" ]]; then
    annotations="annotations:
    dev.gvisor.internal.restore.host-image-path: ${checkpoint}
    dev.gvisor.internal.restore.background: \"true\""
  fi
  on_node kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NS}
  ${annotations}
spec:
  nodeName: ${NODE}
  runtimeClassName: gvisor
  restartPolicy: Never
  automountServiceAccountToken: false
  enableServiceLinks: false
  terminationGracePeriodSeconds: 0
  containers:
    - name: counter
      image: ${IMAGE}
      command:
        - sh
        - -c
        - '${WORKLOAD}'
      resources:
        limits:
          memory: ${memory}
EOF
}

# running NAME waits for the pod to run, and prints how long that took.
running() {
  local -r name="$1"
  local -r start="$(date +%s%N)"
  local ms
  until [[ "$(on_node kubectl -n "${NS}" get pod "${name}" \
    -o 'jsonpath={.status.phase} {.status.containerStatuses[0].ready}')" == "Running true" ]]; do
    ms=$(( ($(date +%s%N) - start) / 1000000 ))
    if (( ms > 120000 )); then
      echo "pod ${name} is not running after 2 minutes" >&2
      return 1
    fi
    sleep 0.05
  done
  ms=$(( ($(date +%s%N) - start) / 1000000 ))
  printf '%d.%03d\n' $(( ms / 1000 )) $(( ms % 1000 ))
}

# pod_exec NAME COMMAND... runs COMMAND in pod NAME.
pod_exec() {
  on_node kubectl -n "${NS}" exec "$1" -- "${@:2}"
}

# ctr_exec NAME COMMAND... runs COMMAND in the containerd container NAME.
ctr_exec() {
  on_node ctr --namespace "${NS}" task exec --exec-id "exec-${RANDOM}" "$1" "${@:2}"
}

sandbox_id() {
  on_node crictl pods --quiet --name "$1" --state ready
}

# counting NAME [EXEC] waits until pod NAME (or the container that EXEC,
# pod_exec by default, runs commands in) has counted for a second.
counting() {
  local -r name="$1" exec="${2:-pod_exec}"
  until "${exec}" "${name}" cat /count >/dev/null 2>&1; do
    sleep 0.2
  done
  sleep 1
}

# restored NAME BEFORE [EXEC] checks that pod NAME (or the container that EXEC,
# pod_exec by default, runs commands in) counts on from BEFORE, the count of
# the workload it was restored from at its checkpoint: the same start token, a
# count at least as high, counting, and its data intact.
restored() {
  local -r name="$1" before="$2" exec="${3:-pod_exec}"
  local after later data
  after="$("${exec}" "${name}" cat /count)"
  sleep 1
  later="$("${exec}" "${name}" cat /count)"
  data="$("${exec}" "${name}" md5sum /dev/shm/data)"
  echo "${name}: ${before} at the checkpoint; ${after}, then ${later} after the restore"
  if [[ "${after%% *}" != "${before%% *}" ]]; then
    echo "${name} started afresh instead of being restored" >&2
    return 1
  fi
  if (( ${after##* } < ${before##* } || ${later##* } <= ${after##* } )); then
    echo "${name} does not count on from its checkpoint" >&2
    return 1
  fi
  if [[ "${data:0:16}" != "${before%% *}" ]]; then
    echo "${name}'s data changed: its digest is ${data:0:16}" >&2
    return 1
  fi
}

on_node mkdir -p /checkpoints
pod source 256Mi
echo "Source pod running after $(running source) s."
counting source

# A checkpoint that leaves the sandbox running, restored into a clone.
before="$(pod_exec source cat /count)"
on_node ctr --namespace k8s.io task checkpoint --image-path /checkpoints/live "$(sandbox_id source)"
pod clone 256Mi /checkpoints/live
echo "Clone running after $(running clone) s."
restored clone "${before}"
pod_exec source cat /count >/dev/null
on_node kubectl -n "${NS}" delete pod clone --grace-period=0 --force >/dev/null 2>&1

# A checkpoint that stops the sandbox, restored into a new pod, then into one
# with another memory limit.
before="$(pod_exec source cat /count)"
on_node ctr --namespace k8s.io task checkpoint --exit --image-path /checkpoints/exit "$(sandbox_id source)"
pod restored 256Mi /checkpoints/exit
echo "Restored pod running after $(running restored) s."
restored restored "${before}"
on_node kubectl -n "${NS}" delete pod restored --grace-period=0 --force >/dev/null 2>&1
pod resized 512Mi /checkpoints/exit
echo "Pod with a new memory limit running after $(running resized) s."
restored resized "${before}"

# A checkpoint that does not exist: the pod must not start.
pod missing 256Mi /checkpoints/none
sleep 20
phase="$(on_node kubectl -n "${NS}" get pod missing -o 'jsonpath={.status.phase}')"
events="$(on_node kubectl -n "${NS}" get events --field-selector involvedObject.name=missing \
  -o 'jsonpath={range .items[*]}{.reason}: {.message}{"\n"}{end}')"
if [[ "${phase}" != Pending ]] || ! grep -q /checkpoints/none <<<"${events}"; then
  echo "a pod restored from a missing checkpoint is ${phase}; its events:" >&2
  echo "${events}" >&2
  exit 1
fi
echo "Pod with a missing checkpoint: ${phase}, $(grep -m 1 /checkpoints/none <<<"${events}" | cut -c 1-200)"

# A plain containerd client: a checkpoint in containerd's content store.
on_node ctr --namespace "${NS}" images pull "${IMAGE}" >/dev/null
on_node ctr --namespace "${NS}" run --detach --runtime io.containerd.runsc.v1 \
  --runtime-config-path /etc/containerd/runsc/runsc.toml "${IMAGE}" plain sh -c "${WORKLOAD}"
counting plain ctr_exec
before="$(ctr_exec plain cat /count)"
on_node ctr --namespace "${NS}" containers checkpoint --task plain checkpoint/plain:1
on_node ctr --namespace "${NS}" task kill --signal KILL plain
until on_node ctr --namespace "${NS}" task delete plain >/dev/null 2>&1; do
  sleep 0.2
done
on_node ctr --namespace "${NS}" containers delete plain
on_node ctr --namespace "${NS}" containers restore --live plain-restored checkpoint/plain:1
restored plain-restored "${before}" ctr_exec

echo "Checkpoint and restore through the shim: ok"
