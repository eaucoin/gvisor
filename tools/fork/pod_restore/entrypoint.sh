#!/bin/sh

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

# The entrypoint of pod_restore_test.sh's k3s node container: it readies the
# container for kubelet, then runs k3s with the container's arguments.

set -eu

# cgroup v2, in the container's own cgroup namespace: a cgroup that holds
# processes cannot hand controllers down to the cgroups below it, as kubelet's
# need, so the container's processes move out of the root into /init and the
# root hands every controller down, as Docker-in-Docker does.
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs || :
  sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control
fi

# The root's mounts shared, so that the mounts kubelet makes for pods
# propagate as they ask.
mount --make-rshared /

exec /bin/k3s "$@"
