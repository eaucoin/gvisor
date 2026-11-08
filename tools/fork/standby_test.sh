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

# standby_test.sh checks restores into standby sandboxes (the shim's
# standby.go) in Kubernetes, and times them against restores into pods made
# for them, as pod_restore_test.sh makes them: a one-node k3s cluster, in a
# Docker container, runs a pod that counts in its memory under runsc; its
# sandbox is checkpointed, stopping it; then, RUNS times each, alternately:
#
#   - a pod is made with the restore annotations, and timed from its creation
#     to its restored workload's first count read back;
#   - a standby pod is made, and once it runs, a restore is asked of its shim
#     on its socket, timed from the request to the restored workload's first
#     count read back.
#
# Each must count on from its checkpointed state. Then: a standby restored
# paused counts nothing until its containers are resumed; a second restore of
# a standby is refused; and a standby deleted before any restore goes.
#
# It needs root (through sudo), Docker and curl.
#
# Usage: tools/fork/standby_test.sh DIR RESULTS [RUNSC_FLAG...]
#
# DIR holds runsc, its gvisor-bin/ and containerd-shim-runsc-v1, as a release
# tarball unpacks; RESULTS is the JSON lines file the times are written to;
# each RUNSC_FLAG, --name=value, is set for every sandbox.

set -euo pipefail

declare dir
dir="$(realpath "${1:?usage: standby_test.sh DIR RESULTS [RUNSC_FLAG...]}")"
readonly DIR="${dir}"
readonly RESULTS="${2:?usage: standby_test.sh DIR RESULTS [RUNSC_FLAG...]}"
shift 2
readonly RUNS="${STANDBY_RUNS:-20}"
# With STANDBY_GROUPING=false, each container has a shim of its own, as
# containerd runs the shim by default: the sandbox's shim serves the root
# container's socket, and each other container's shim its own.
readonly GROUPING="${STANDBY_GROUPING:-true}"

readonly K3S="mirror.gcr.io/rancher/k3s:v1.37.1-k3s1@sha256:ca7f37d993d82ef0dcdcfecb2e0e2618ea541dbaffc620c8cedebe01a82acd0d"
readonly IMAGE="mirror.gcr.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
readonly NODE="standby-$$"
readonly NS="standby"
# The workload of pod_restore_test.sh: each count is renamed over /count, so
# that no read of it lands between its truncation and the next count's write.
# shellcheck disable=SC2016 # The container's shell expands it.
readonly WORKLOAD='head -c 50331648 /dev/urandom >/dev/shm/data; start=$(md5sum </dev/shm/data | cut -c 1-16); i=0; while :; do i=$((i+1)); echo "$start $i" >/count.new; mv /count.new /count; sleep 0.2; done'
declare conf
conf="$(dirname "$(realpath "$0")")/pod_restore"
readonly CONF="${conf}"

declare tmp
tmp="$(mktemp -d)"
readonly tmp
# The node's /run/standby, where the standbys' shims listen, which this
# script asks for restores through.
readonly SOCKETS="${tmp}/standby"
mkdir -p "${SOCKETS}"

for f in runsc gvisor-bin containerd-shim-runsc-v1; do
  if [[ ! -e "${DIR}/${f}" ]]; then
    echo "${DIR}/${f} does not exist" >&2
    exit 1
  fi
done

{
  echo 'binary_name = "/opt/gvisor/runsc"'
  echo 'restore_background = true'
  echo 'log_path = "/var/log/runsc/shim/%ID%.log"'
  echo 'log_level = "debug"'
  echo '[runsc_config]'
  echo '  debug-log = "/var/log/runsc/%ID%/"'
  echo '  debug-command = "boot"'
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
    on_node kubectl -n "${NS}" get pods -o wide >&2 || true
    on_node kubectl -n "${NS}" get events --sort-by=.lastTimestamp >&2 || true
    on_node sh -c 'find /var/log/runsc -name "*.boot.txt" -exec tail -n 30 {} +' >&2 || true
    on_node sh -c 'grep -h -i -E "standby|Start, id|Kill, id|exit" /var/log/runsc/shim/*.log | tr -d "\\000" | sort | tail -n 80' >&2 || true
    sudo dmesg | tail -n 30 >&2 || true
    on_node tail -n 80 /var/lib/rancher/k3s/agent/containerd/containerd.log >&2 || true
    sudo docker logs --tail 50 "${NODE}" >&2 || true
  fi
  sudo docker rm -f "${NODE}" >/dev/null 2>&1 || true
  sudo rm -rf "${tmp}"
}
trap cleanup EXIT

sed "s/^grouping = .*/grouping = ${GROUPING}/" "${CONF}/config.toml" >"${tmp}/config.toml"

sudo docker run --detach --name "${NODE}" --hostname "${NODE}" --privileged \
  --memory 1536m --memory-swap 1536m --tmpfs /run --tmpfs /var/run \
  --volume "${SOCKETS}:/run/standby" \
  --volume "${CONF}/entrypoint.sh:/usr/local/bin/entrypoint.sh:ro" \
  --volume "${CONF}/config-v3.toml.tmpl:/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl:ro" \
  --volume "${tmp}/config.toml:/etc/containerd/runsc/config.toml:ro" \
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

# pod NAME [ANNOTATION=VALUE...] applies a pod that runs the workload, with
# the given annotations. Restored pods repeat the source's spec.
pod() {
  local -r name="$1"
  shift
  local annotations=""
  if (( $# > 0 )); then
    annotations="annotations:"
    for annotation in "$@"; do
      annotations+="
    ${annotation%%=*}: \"${annotation#*=}\""
    done
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
          memory: 256Mi
EOF
}

now_ms() {
  echo $(( $(date +%s%N) / 1000000 ))
}

# running NAME waits for the pod's containers to run, by the API.
running() {
  local -r name="$1" start="$(now_ms)"
  until [[ "$(on_node kubectl -n "${NS}" get pod "${name}" \
    -o 'jsonpath={.status.phase} {.status.containerStatuses[0].ready}')" == "Running true" ]]; do
    if (( $(now_ms) - start > 120000 )); then
      echo "pod ${name} is not running after 2 minutes" >&2
      return 1
    fi
    sleep 0.05
  done
}

pod_exec() {
  on_node kubectl -n "${NS}" exec "$1" -- "${@:2}"
}

sandbox_id() {
  on_node crictl pods --quiet --name "$1" --state ready
}

# counted NAME BEFORE waits until the workload of pod NAME has counted on from
# BEFORE, its count at the checkpoint: its first count read back.
counted() {
  local -r name="$1" before="$2" start="$(now_ms)"
  local count
  until count="$(pod_exec "${name}" cat /count 2>/dev/null)" && [[ "${count%% *}" == "${before%% *}" ]]; do
    if (( $(now_ms) - start > 120000 )); then
      echo "pod ${name} has not counted on from ${before} after 2 minutes: ${count:-nothing read}" >&2
      return 1
    fi
    sleep 0.02
  done
}

# restored NAME BEFORE checks that pod NAME counts on from BEFORE, with its
# data intact.
restored() {
  local -r name="$1" before="$2"
  local after later data
  after="$(pod_exec "${name}" cat /count)"
  sleep 1
  later="$(pod_exec "${name}" cat /count)"
  data="$(pod_exec "${name}" md5sum /dev/shm/data)"
  if [[ "${after%% *}" != "${before%% *}" ]] || (( ${after##* } < ${before##* } || ${later##* } <= ${after##* } )); then
    echo "${name} does not count on from its checkpoint: ${before}, then ${after}, ${later}" >&2
    return 1
  fi
  if [[ "${data:0:16}" != "${before%% *}" ]]; then
    echo "${name}'s data changed: its digest is ${data:0:16}" >&2
    return 1
  fi
}

# socket NAME waits for the socket of standby pod NAME, which its shim makes as
# it starts it.
socket() {
  local -r start="$(now_ms)"
  until sudo test -S "${SOCKETS}/$1.sock"; do
    if (( $(now_ms) - start > 60000 )); then
      echo "standby $1 has no socket after a minute" >&2
      return 1
    fi
    sleep 0.05
  done
}

# restore_standby NAME BODY asks the shims of standby pod NAME to restore, as
# BODY says: the sandbox's, then each container's of its own where shim
# grouping is off. Its answer is a JSON array of theirs.
restore_standby() {
  local -a answers=()
  local socket
  for socket in "${SOCKETS}/$1.sock" $(sudo find "${SOCKETS}" -maxdepth 1 -name "$1.*.sock" | sort); do
    answers+=("$(sudo curl --silent --show-error --fail-with-body --max-time 120 --unix-socket "${socket}" \
      --data "$2" http://standby/restore)") || return 1
  done
  local IFS=,
  echo "[${answers[*]}]"
}

# boot_lines NAME: the restore's lines of pod NAME's boot log.
boot_lines() {
  on_node sh -c "cat /var/log/runsc/$(sandbox_id "$1")/runsc.log.*.boot.txt 2>/dev/null" |
    grep -E 'Restore successful|Restored container|restore cont|Kernel load took|VFS restored|Starting sandbox' | head -n 20 || true
}

# gone NAME deletes pod NAME, and waits for kubelet to have stopped it.
gone() {
  on_node kubectl -n "${NS}" delete pod "$1" --wait=true --timeout=90s >/dev/null
}

on_node mkdir -p /checkpoints
pod source
running source
until pod_exec source cat /count >/dev/null 2>&1; do sleep 0.2; done
sleep 1
before="$(pod_exec source cat /count)"
on_node ctr --namespace k8s.io task checkpoint --exit --image-path /checkpoints/exit "$(sandbox_id source)"
echo "Checkpointed the source at ${before}."
: >"${RESULTS}"

for (( run = 1; run <= RUNS; run++ )); do
  # A pod made for the restore.
  start="$(now_ms)"
  pod "made-${run}" dev.gvisor.internal.restore.host-image-path=/checkpoints/exit dev.gvisor.internal.restore.background=true
  running "made-${run}"
  ran="$(now_ms)"
  counted "made-${run}" "${before}"
  read_back="$(now_ms)"
  restored "made-${run}" "${before}"
  echo "{\"run\": ${run}, \"kind\": \"made\", \"running_ms\": $(( ran - start )), \"counted_ms\": $(( read_back - start ))}" >>"${RESULTS}"
  echo "made-${run}: running after $(( ran - start )) ms, counted after $(( read_back - start )) ms"
  if (( run == 1 )); then boot_lines "made-${run}"; fi
  gone "made-${run}"

  # A standby, restored into once it runs.
  made="$(now_ms)"
  pod "standby-${run}" "dev.gvisor.internal.restore.standby=/run/standby/standby-${run}.sock"
  running "standby-${run}"
  socket "standby-${run}"
  start="$(now_ms)"
  answer="$(restore_standby "standby-${run}" '{"imagePath": "/checkpoints/exit"}')"
  answered="$(now_ms)"
  counted "standby-${run}" "${before}"
  read_back="$(now_ms)"
  restored "standby-${run}" "${before}"
  echo "{\"run\": ${run}, \"kind\": \"standby\", \"standby_running_ms\": $(( start - made )), \"answered_ms\": $(( answered - start )), \"counted_ms\": $(( read_back - start )), \"shim\": ${answer}}" >>"${RESULTS}"
  echo "standby-${run}: running after $(( start - made )) ms; restore answered after $(( answered - start )) ms (${answer}), counted after $(( read_back - start )) ms"
  if (( run == 1 )); then boot_lines "standby-${run}"; fi
  # Single use.
  if restore_standby "standby-${run}" '{"imagePath": "/checkpoints/exit"}' >/dev/null 2>&1; then
    echo "standby-${run} took a second restore" >&2
    exit 1
  fi
  gone "standby-${run}"
done

# Restored paused: it counts nothing until resumed.
pod paused "dev.gvisor.internal.restore.standby=/run/standby/paused.sock"
running paused
socket paused
restore_standby paused '{"imagePath": "/checkpoints/exit", "paused": true}' >/dev/null
sleep 2
# An exec into a paused sandbox runs nothing until it is resumed.
if count="$(on_node timeout 3 kubectl -n "${NS}" exec paused -- cat /count 2>/dev/null)" && [[ "${count}" != "${before}" ]]; then
  echo "a standby restored paused counted before it was resumed: ${count}" >&2
  exit 1
fi
for id in $(on_node crictl ps --quiet --pod "$(sandbox_id paused)"); do
  on_node ctr --namespace k8s.io task resume "${id}" || true
done
on_node ctr --namespace k8s.io task resume "$(sandbox_id paused)" || true
counted paused "${before}"
restored paused "${before}"
echo "A standby restored paused counts once resumed."
gone paused

# A standby deleted before any restore goes, its containers' exits reported.
pod unused "dev.gvisor.internal.restore.standby=/run/standby/unused.sock"
running unused
socket unused
start="$(now_ms)"
gone unused
echo "An unused standby deleted in $(( $(now_ms) - start )) ms."

echo "Restores into standby sandboxes: ok"
