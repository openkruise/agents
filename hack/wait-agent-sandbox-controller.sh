#!/usr/bin/env bash
# Copyright (c) 2025 Alibaba Group Holding Ltd.

# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at

#      http://www.apache.org/licenses/LICENSE-2.0

# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -ex

kubectl cluster-info

for ((i=1;i<10;i++));
do
  set +e
  PODS=$(kubectl get pod -n sandbox-system | grep -c '1/1')
  set -e
  if [ "$PODS" -eq "1" ]; then
    break
  fi
  sleep 3
done

set +e
PODS=$(kubectl get pod -n sandbox-system | grep -c '1/1')
kubectl get node -o yaml
kubectl get all -n sandbox-system -o yaml
kubectl get pod -n sandbox-system --no-headers | awk '{print $1}' | xargs kubectl describe pods -n sandbox-system
kubectl get pod -n sandbox-system --no-headers | awk '{print $1}' | xargs kubectl logs -n sandbox-system
kubectl get pod -n sandbox-system --no-headers | awk '{print $1}' | xargs kubectl logs -n sandbox-system --previous=true
set -e
if [ "$PODS" -eq "1" ]; then
  echo "Wait for agent-sandbox-controller ready successfully"
else
  echo "Timeout to wait for agent-sandbox-controller ready"
  exit 1
fi
