#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail

go mod vendor
retVal=$?
if [ $retVal -ne 0 ]; then
    exit $retVal
fi

TMP_DIR=$(mktemp -d)
mkdir -p "${TMP_DIR}"/src/gitlab.alibaba-inc.com/serverlessinfra/agents/client
cp -r ./{api,hack,vendor,go.mod} "${TMP_DIR}"/src/gitlab.alibaba-inc.com/serverlessinfra/agents/

chmod +x "${TMP_DIR}"/src/gitlab.alibaba-inc.com/serverlessinfra/agents/vendor/k8s.io/code-generator/generate-internal-groups.sh
echo "tmp_dir: ${TMP_DIR}"

SCRIPT_ROOT="${TMP_DIR}"/src/gitlab.alibaba-inc.com/serverlessinfra/agents
CODEGEN_PKG=${CODEGEN_PKG:-"${SCRIPT_ROOT}/vendor/k8s.io/code-generator"}

echo "source ${CODEGEN_PKG}/kube_codegen.sh"
source "${CODEGEN_PKG}/kube_codegen.sh"

echo "gen_client"
GOPATH=${TMP_DIR} GO111MODULE=off kube::codegen::gen_client \
    --with-watch \
    --output-dir "${SCRIPT_ROOT}/client" \
    --output-pkg "gitlab.alibaba-inc.com/serverlessinfra/agents/client" \
    --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
    "${SCRIPT_ROOT}"

rm -rf ./client/{clientset,informers,listers}
mv "${TMP_DIR}"/src/gitlab.alibaba-inc.com/serverlessinfra/agents/client/* ./client
rm -rf vendor