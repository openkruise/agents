# Peer mTLS E2E fixtures
#
# CI-only Kind/Kustomize helpers for `.github/workflows/e2e-e2b-latest.yaml`
# matrix row `with sandbox-gateway, peer mTLS`. These manifests are not
# production Helm or the default Gateway security mode.
#
# The row enables peer TLS on Manager and Gateway, then checks inbound
# /refresh rejection with curl. It does not deploy a third peer or a
# custom helper image.
