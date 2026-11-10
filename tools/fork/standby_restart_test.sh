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

# standby_restart_test.sh checks that standby sandboxes (the shim's
# standby.go) survive a restart of containerd, as a restart of k3s restarts
# it, its shims running on: in a one-node k3s cluster in a Docker container,
# as standby_test.sh makes it, k3s is restarted, its containerd with it, with
# each of these on the node:
#
#   - a standby, running, that no restore was made into, which then takes one;
#   - a standby restored into, which counts on through the restart;
#   - a standby whose container was stopped, its pod kept, as kubelet keeps
#     the stopped containers of a pod that never restarts them;
#   - a standby being deleted.
#
# After each restart, containerd's CRI must have recovered every container
# within RECOVERY_SECONDS (crictl answers, the node is Ready), and the pods
# must be as they were. A recovery that does not end dumps containerd's
# goroutines.
#
# It needs root (through sudo), Docker and curl.
#
# Usage: tools/fork/standby_restart_test.sh DIR [RUNSC_FLAG...]
#
# DIR holds runsc, its gvisor-bin/ and containerd-shim-runsc-v1, as a release
# tarball unpacks; each RUNSC_FLAG, --name=value, is set for every sandbox.

set -euo pipefail

declare dir
dir="$(realpath "${1:?usage: standby_restart_test.sh DIR [RUNSC_FLAG...]}")"
readonly DIR="${dir}"
shift
# With STANDBY_GROUPING=false, each container has a shim of its own, as
# containerd runs the shim by default.
readonly GROUPING="${STANDBY_GROUPING:-true}"
readonly RECOVERY_SECONDS="${RECOVERY_SECONDS:-180}"

readonly K3S="mirror.gcr.io/rancher/k3s:v1.37.1-k3s1@sha256:ca7f37d993d82ef0dcdcfecb2e0e2618ea541dbaffc620c8cedebe01a82acd0d"
readonly IMAGE="mirror.gcr.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
readonly NODE="standby-restart-$$"
readonly NS="standby"
# The workload of standby_test.sh.
# shellcheck disable=SC2016 # The container's shell expands it.
readonly WORKLOAD='head -c 16777216 /dev/urandom >/dev/shm/data; start=$(md5sum </dev/shm/data | cut -c 1-16); i=0; while :; do i=$((i+1)); echo "$start $i" >/count.new; mv /count.new /count; sleep 0.2; done'
declare conf
conf="$(dirname "$(realpath "$0")")/pod_restore"
readonly CONF="${conf}"

declare tmp
tmp="$(mktemp -d)"
readonly tmp
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

# The node's entrypoint: pod_restore_test.sh's, but k3s run again each time it
# exits, so that a restart of it leaves the node's container, and the shims in
# it, running, as systemd restarts k3s on a host (KillMode=process).
cat >"${tmp}/entrypoint.sh" <<'EOF'
#!/bin/sh
set -eu
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs || :
  sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control
fi
mount --make-rshared /
while :; do
  /bin/k3s "$@" &
  echo $! >/run/k3s.pid
  wait $! || :
  sleep 1
done
EOF
chmod +x "${tmp}/entrypoint.sh"

on_node() {
  sudo docker exec -i "${NODE}" "$@"
}

# goroutines says containerd's goroutines of its CRI plugin and of the shims'
# clients, as SIGUSR1 has it dump them into its log.
goroutines() {
  on_node sh -c 'pkill -USR1 -x containerd; sleep 3; awk "/BEGIN goroutine stack dump/{on=1} on{print} /END goroutine stack dump/{on=0}" /var/lib/rancher/k3s/agent/containerd/containerd.log' |
    sed 's/\\n/\n/g; s/\\t/  /g' | awk -v RS='goroutine [0-9]+ ' '/internal\/cri\/server|client\/task|cri\/io/ && !/waitSandboxExit|ExitMonitor|StatsCollector/ { print "goroutine " $0 }' | head -n 300 >&2 || true
}

cleanup() {
  local status=$?
  if (( status != 0 )); then
    on_node kubectl -n "${NS}" get pods -o wide >&2 || true
    on_node sh -c 'grep -h -i -E "standby|Start, id|Kill, id|Delete, id|State, id|exit" /var/log/runsc/shim/*.log | tr -d "\\000" | sort | tail -n 120' >&2 || true
    on_node tail -n 60 /var/lib/rancher/k3s/agent/containerd/containerd.log >&2 || true
    sudo docker logs --tail 40 "${NODE}" >&2 || true
  fi
  sudo docker rm -f "${NODE}" >/dev/null 2>&1 || true
  sudo rm -rf "${tmp}"
}
trap cleanup EXIT

sed "s/^grouping = .*/grouping = ${GROUPING}/" "${CONF}/config.toml" >"${tmp}/config.toml"

sudo docker run --detach --name "${NODE}" --hostname "${NODE}" --privileged \
  --memory 2g --memory-swap 2g --tmpfs /run --tmpfs /var/run \
  --volume "${SOCKETS}:/run/standby" \
  --volume "${tmp}/entrypoint.sh:/usr/local/bin/entrypoint.sh:ro" \
  --volume "${CONF}/config-v3.toml.tmpl:/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl:ro" \
  --volume "${tmp}/config.toml:/etc/containerd/runsc/config.toml:ro" \
  --volume "${tmp}/runsc.toml:/etc/containerd/runsc/runsc.toml:ro" \
  --volume "${DIR}:/opt/gvisor:ro" \
  --volume "${DIR}/containerd-shim-runsc-v1:/bin/containerd-shim-runsc-v1:ro" \
  --entrypoint /usr/local/bin/entrypoint.sh "${K3S}" \
  server --disable=traefik,servicelb,metrics-server,local-storage --disable-helm-controller \
  --node-name="${NODE}" >/dev/null

now_ms() {
  echo $(( $(date +%s%N) / 1000000 ))
}

# up SECONDS waits for containerd's CRI to answer and the node to be Ready,
# SECONDS at most.
up() {
  local -r start="$(now_ms)"
  until on_node crictl --timeout 5s info >/dev/null 2>&1 &&
    on_node kubectl get node "${NODE}" --no-headers 2>/dev/null | grep -qw Ready; do
    if (( $(now_ms) - start > $1 * 1000 )); then
      echo "containerd's CRI, or the node, is not up after $1 s" >&2
      goroutines
      return 1
    fi
    sleep 1
  done
}

up 360
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
echo "Node ready: $(on_node k3s --version | head -n 1); $(on_node /opt/gvisor/runsc --version | head -n 1); shim grouping ${GROUPING}."

# restart restarts k3s, and containerd with it, the shims running on, and
# waits for containerd's CRI to have recovered and the node to be Ready again.
restart() {
  local -r start="$(now_ms)"
  # The node's status as it was, so that Ready again is told by a status
  # posted since.
  on_node sh -c 'kill -TERM "$(cat /run/k3s.pid)"'
  until ! on_node sh -c 'kill -0 "$(cat /run/k3s.pid)"' 2>/dev/null; do
    sleep 0.5
  done
  sleep 5
  up "${RECOVERY_SECONDS}"
  echo "k3s restarted ($1), containerd's CRI and the node up again after $(( $(now_ms) - start )) ms."
}

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

running() {
  local -r name="$1" start="$(now_ms)"
  until [[ "$(on_node kubectl -n "${NS}" get pod "${name}" \
    -o 'jsonpath={.status.phase} {.status.containerStatuses[0].ready}')" == "Running true" ]]; do
    if (( $(now_ms) - start > 120000 )); then
      echo "pod ${name} is not running after 2 minutes" >&2
      return 1
    fi
    sleep 0.1
  done
}

pod_exec() {
  on_node kubectl -n "${NS}" exec "$1" -- "${@:2}"
}

sandbox_id() {
  on_node crictl pods --quiet --name "$1" --state ready
}

# counts NAME BEFORE checks that pod NAME counts on from BEFORE, the count of
# the checkpoint, its count growing.
counts() {
  local -r name="$1" before="$2" start="$(now_ms)"
  local after later
  until after="$(pod_exec "${name}" cat /count 2>/dev/null)" && [[ "${after%% *}" == "${before%% *}" ]]; do
    if (( $(now_ms) - start > 120000 )); then
      echo "pod ${name} has not counted on from ${before} after 2 minutes: ${after:-nothing read}" >&2
      return 1
    fi
    sleep 0.1
  done
  sleep 1
  later="$(pod_exec "${name}" cat /count)"
  if (( ${after##* } < ${before##* } || ${later##* } <= ${after##* } )); then
    echo "${name} does not count on from ${before}: ${after}, then ${later}" >&2
    return 1
  fi
}

socket() {
  local -r start="$(now_ms)"
  until sudo test -S "${SOCKETS}/$1.sock"; do
    if (( $(now_ms) - start > 60000 )); then
      echo "standby $1 has no socket after a minute" >&2
      return 1
    fi
    sleep 0.1
  done
}

restore_standby() {
  local socket
  for socket in "${SOCKETS}/$1.sock" $(sudo find "${SOCKETS}" -maxdepth 1 -name "$1.*.sock" | sort); do
    sudo curl --silent --show-error --fail-with-body --max-time 120 --unix-socket "${socket}" \
      --data '{"imagePath": "/checkpoints/exit"}' http://standby/restore >/dev/null || return 1
  done
}

standby() {
  pod "$1" "dev.gvisor.internal.restore.standby=/run/standby/$1.sock"
  running "$1"
  socket "$1"
}

on_node mkdir -p /checkpoints
pod source
running source
until pod_exec source cat /count >/dev/null 2>&1; do sleep 0.2; done
sleep 1
before="$(pod_exec source cat /count)"
on_node ctr --namespace k8s.io task checkpoint --exit --image-path /checkpoints/exit "$(sandbox_id source)"
echo "Checkpointed the source at ${before}."

# A standby that no restore was made into: it runs on, then takes a restore.
standby idle
restart "an idle standby"
running idle
restore_standby idle
counts idle "${before}"
echo "An idle standby runs on through the restart, and takes a restore after it."

# A standby restored into counts on through the restart.
standby restored
restore_standby restored
counts restored "${before}"
restart "a restored standby"
counts restored "${before}"
echo "A restored standby counts on through the restart."

# A standby whose container was stopped, its pod kept: stopped as kubelet
# stops one, CRI's stop waiting for its exit and its I/O's end (a minute at
# most, as the first deletion of a container that never ran may fail, and CRI
# tries it again).
standby stopped
for id in $(on_node crictl ps --quiet --pod "$(sandbox_id stopped)"); do
  on_node crictl --timeout 60s stop --timeout 0 "${id}" >/dev/null
done
until [[ "$(on_node kubectl -n "${NS}" get pod stopped -o 'jsonpath={.status.containerStatuses[0].state.terminated.reason}')" != "" ]]; do
  sleep 0.5
done
restart "a standby whose container was stopped"
on_node kubectl -n "${NS}" delete pod stopped --wait=true --timeout=90s >/dev/null
echo "A standby whose container was stopped is recovered, and goes once deleted."

# A standby being deleted.
standby deleting
on_node kubectl -n "${NS}" delete pod deleting --wait=false >/dev/null
restart "a standby being deleted"
on_node kubectl -n "${NS}" wait --for=delete pod/deleting --timeout=90s >/dev/null
echo "A standby deleted as k3s restarts goes."

for name in idle restored; do
  on_node kubectl -n "${NS}" delete pod "${name}" --wait=true --timeout=90s >/dev/null
done
echo "Standby sandboxes through restarts of k3s: ok"
