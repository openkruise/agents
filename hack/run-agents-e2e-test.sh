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

set -e
# default value
FOCUS_DEFAULT='Sandbox'
FOCUS=${FOCUS_DEFAULT}
SKIP=""
TIMEOUT="60m"
PRINT_INFO=false
PARALLEL=false

while [[ $# -gt 0 ]]; do
    key="$1"
    case $key in
        --focus)
            FOCUS="$2"
            shift # past argument
            shift # past value
            ;;
        --skip)
            if [ -z "$SKIP" ]; then
                SKIP="$2"
            else
                SKIP="$SKIP|$2"
            fi
            shift # past argument
            shift # past value
            ;;
        --timeout)
            TIMEOUT="$2"
            shift # past argument
            shift # past value
            ;;
        --print-info)
            PRINT_INFO=true
            shift # past argument
            ;;
        --disable-parallel)
            PARALLEL=false
            shift # past argument
            ;;
        *)
            echo "Unknown parameter passed: $1"
            exit 1
            ;;
    esac
done

config=${KUBECONFIG:-${HOME}/.kube/config}
export KUBECONFIG=${config}
make ginkgo

set +e
set -x
GINKGO_CMD="./bin/ginkgo -timeout $TIMEOUT -v --fail-fast"
if [ "$PARALLEL" = true ]; then
    GINKGO_CMD+=" -p"
fi
if [ -n "$FOCUS" ]; then
    GINKGO_CMD+=" --focus='$FOCUS'"
fi
if [ -n "$SKIP" ]; then
    GINKGO_CMD+=" --skip='$SKIP'"
fi
GINKGO_CMD+=" test/e2e"

bash -c "$GINKGO_CMD"
retVal=$?
restartCount=$(kubectl get pod -n sandbox-system -l control-plane=sandbox-controller-manager --no-headers | awk '{print $4}')
if [ "${restartCount}" -eq "0" ];then
    echo "agent-sandbox-controller has not restarted"
else
    kubectl get pod -n sandbox-system --no-headers -l control-plane=sandbox-controller-manager
    echo "agent-sandbox-controller has restarted, abort!!!"
    kubectl get pod -n sandbox-system -l control-plane=sandbox-controller-manager --no-headers | awk '{print $1}' | xargs kubectl logs -p -n sandbox-system
    exit 1
fi

if [ "$PRINT_INFO" = true ]; then
    if [ "$retVal" -ne 0 ];then
        echo "test fail, dump kruise-manager logs"
        while read pod; do
             kubectl logs -n sandbox-system $pod
        done < <(kubectl get pods -n sandbox-system -l control-plane=sandbox-controller-manager  --no-headers | awk '{print $1}')
    fi
fi

exit $retVal
