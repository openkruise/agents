#!/usr/bin/env python3
"""Black-box tests for the sync-charts drift checker."""

from __future__ import annotations

import copy
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import yaml


SKILL_DIR = Path(__file__).resolve().parents[1]
CHECKER = SKILL_DIR / "scripts" / "chart_drift.py"
SKILL = SKILL_DIR / "SKILL.md"


class ChartDriftTest(unittest.TestCase):
    def test_reports_kustomization_crd_without_chart_mapping(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            (agents_repo / "config" / "crd" / "bases").mkdir(parents=True)
            (charts_repo / "versions").mkdir(parents=True)
            (agents_repo / "config" / "crd" / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_unmappedfixtures.yaml\n",
                encoding="utf-8",
            )
            (
                agents_repo / "config" / "crd" / "bases" / "agents.kruise.io_unmappedfixtures.yaml"
            ).write_text(
                "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n",
                encoding="utf-8",
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn(
            "UNMAPPED crd agents.kruise.io_unmappedfixtures.yaml",
            result.stdout,
        )

    def test_reports_missing_charts_checkout_as_configuration_error(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            source = (
                agents_repo
                / "config"
                / "crd"
                / "bases"
                / "agents.kruise.io_checkpoints.yaml"
            )
            source.parent.mkdir(parents=True)
            (agents_repo / "config" / "crd" / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_checkpoints.yaml\n",
                encoding="utf-8",
            )
            source.write_text(
                "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n",
                encoding="utf-8",
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(root / "missing-charts"),
                    "--aspect",
                    "crd",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("ERROR missing charts checkout", result.stderr)

    def test_reports_mapped_drift_alongside_unmapped_crd(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            crd_dir = agents_repo / "config" / "crd"
            bases_dir = crd_dir / "bases"
            bases_dir.mkdir(parents=True)
            (charts_repo / "versions").mkdir(parents=True)
            (crd_dir / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_unmappedfixtures.yaml\n"
                "- bases/agents.kruise.io_checkpoints.yaml\n",
                encoding="utf-8",
            )
            (bases_dir / "agents.kruise.io_unmappedfixtures.yaml").write_text(
                "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n",
                encoding="utf-8",
            )
            (bases_dir / "agents.kruise.io_checkpoints.yaml").write_text(
                "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n",
                encoding="utf-8",
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("UNMAPPED crd agents.kruise.io_unmappedfixtures.yaml", result.stdout)
        self.assertIn(
            "DRIFT crd controller "
            "versions/kruise-agents-sandbox-controller/next/crds/"
            "agents.kruise.io_checkpoints.yaml missing",
            result.stdout,
        )

    def test_does_not_copy_mapped_crd_when_another_crd_is_unmapped(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            crd_dir = agents_repo / "config" / "crd"
            bases_dir = crd_dir / "bases"
            destination = (
                charts_repo
                / "versions"
                / "kruise-agents-sandbox-controller"
                / "next"
                / "crds"
                / "agents.kruise.io_checkpoints.yaml"
            )
            bases_dir.mkdir(parents=True)
            (charts_repo / "versions").mkdir(parents=True)
            (crd_dir / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_unmappedfixtures.yaml\n"
                "- bases/agents.kruise.io_checkpoints.yaml\n",
                encoding="utf-8",
            )
            (bases_dir / "agents.kruise.io_unmappedfixtures.yaml").write_text(
                "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n",
                encoding="utf-8",
            )
            (bases_dir / "agents.kruise.io_checkpoints.yaml").write_text(
                "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n",
                encoding="utf-8",
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                    "--apply-crds",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertFalse(destination.exists())
        self.assertNotIn("SYNCED crd", result.stdout)

    def test_copies_mapped_crd_when_apply_is_requested(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            source = (
                agents_repo
                / "config"
                / "crd"
                / "bases"
                / "agents.kruise.io_checkpoints.yaml"
            )
            destination = (
                charts_repo
                / "versions"
                / "kruise-agents-sandbox-controller"
                / "next"
                / "crds"
                / "agents.kruise.io_checkpoints.yaml"
            )
            source.parent.mkdir(parents=True)
            destination.parent.mkdir(parents=True)
            (agents_repo / "config" / "crd" / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_checkpoints.yaml\n",
                encoding="utf-8",
            )
            source.write_bytes(b"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n")

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                    "--apply-crds",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(destination.read_bytes(), source.read_bytes())
            self.assertIn("SYNCED crd controller", result.stdout)

    def test_copies_commits_crd_to_controller_chart(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            source = (
                agents_repo
                / "config"
                / "crd"
                / "bases"
                / "agents.kruise.io_commits.yaml"
            )
            destination = (
                charts_repo
                / "versions"
                / "kruise-agents-sandbox-controller"
                / "next"
                / "crds"
                / "agents.kruise.io_commits.yaml"
            )
            source.parent.mkdir(parents=True)
            (charts_repo / "versions").mkdir(parents=True)
            (agents_repo / "config" / "crd" / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_commits.yaml\n",
                encoding="utf-8",
            )
            source.write_bytes(
                b"apiVersion: apiextensions.k8s.io/v1\n"
                b"kind: CustomResourceDefinition\n"
                b"metadata:\n"
                b"  name: commits.agents.kruise.io\n"
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                    "--apply-crds",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(destination.read_bytes(), source.read_bytes())
            self.assertIn(
                "SYNCED crd controller "
                "versions/kruise-agents-sandbox-controller/next/crds/"
                "agents.kruise.io_commits.yaml",
                result.stdout,
            )

    def test_copies_poolautoscalers_crd_to_controller_chart(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            source = (
                agents_repo
                / "config"
                / "crd"
                / "bases"
                / "agents.kruise.io_poolautoscalers.yaml"
            )
            destination = (
                charts_repo
                / "versions"
                / "kruise-agents-sandbox-controller"
                / "next"
                / "crds"
                / "agents.kruise.io_poolautoscalers.yaml"
            )
            source.parent.mkdir(parents=True)
            (charts_repo / "versions").mkdir(parents=True)
            (agents_repo / "config" / "crd" / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_poolautoscalers.yaml\n",
                encoding="utf-8",
            )
            source.write_bytes(
                b"apiVersion: apiextensions.k8s.io/v1\n"
                b"kind: CustomResourceDefinition\n"
                b"metadata:\n"
                b"  name: poolautoscalers.agents.kruise.io\n"
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                    "--apply-crds",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(destination.read_bytes(), source.read_bytes())
            self.assertIn(
                "SYNCED crd controller "
                "versions/kruise-agents-sandbox-controller/next/crds/"
                "agents.kruise.io_poolautoscalers.yaml",
                result.stdout,
            )

    def test_copies_securityprofiles_crd_to_manager_chart(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            agents_repo = root / "agents"
            charts_repo = root / "charts"
            source = (
                agents_repo
                / "config"
                / "crd"
                / "bases"
                / "agents.kruise.io_securityprofiles.yaml"
            )
            destination = (
                charts_repo
                / "versions"
                / "kruise-agents-sandbox-manager"
                / "next"
                / "files"
                / "agentio"
                / "securityprofile-crd.yaml"
            )
            source.parent.mkdir(parents=True)
            (charts_repo / "versions").mkdir(parents=True)
            (agents_repo / "config" / "crd" / "kustomization.yaml").write_text(
                "resources:\n"
                "- bases/agents.kruise.io_securityprofiles.yaml\n",
                encoding="utf-8",
            )
            source.write_bytes(
                b"apiVersion: apiextensions.k8s.io/v1\n"
                b"kind: CustomResourceDefinition\n"
                b"metadata:\n"
                b"  name: securityprofiles.agents.kruise.io\n"
            )

            result = subprocess.run(
                [
                    sys.executable,
                    str(CHECKER),
                    "--agents-repo",
                    str(agents_repo),
                    "--charts-repo",
                    str(charts_repo),
                    "--aspect",
                    "crd",
                    "--apply-crds",
                ],
                check=False,
                capture_output=True,
                text=True,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(destination.read_bytes(), source.read_bytes())
            self.assertIn(
                "SYNCED crd manager "
                "versions/kruise-agents-sandbox-manager/next/files/agentio/"
                "securityprofile-crd.yaml",
                result.stdout,
            )

    def test_documents_identity_resource_synchronization(self) -> None:
        content = SKILL.read_text(encoding="utf-8")

        self.assertIn("## Identity Resources", content)
        self.assertIn("> /tmp/manager-chart.yaml", content)
        for requirement in (
            "controller `templates/rbac.yaml`",
            "manager `templates/rbac.yaml`",
            "preserve `{{ ... }}`",
            "excluding `app.kubernetes.io/managed-by: kustomize`",
            "chart-managed standard `app.kubernetes.io/*` keys win",
            "roleRef `apiGroup` and `kind`",
            "chart's existing namespace helper",
            "source role counterpart",
            "source-rendered binding in the table",
            "exactly one chart counterpart of the same kind",
            "roleRef targets by kind",
            "namePrefix: sandbox-",
            "namespace: sandbox-system",
            "pair bindings by kind",
            "config/sandbox-gateway/jwt-auth-rbac.yaml",
            "config/sandbox-gateway-runtime-mtls/rbac.yaml",
            "Identity resources require manual source-to-rendered review.",
        ):
            with self.subTest(requirement=requirement):
                self.assertIn(requirement, content)
        for source in (
            "config/rbac/service_account.yaml",
            "config/rbac/role_binding.yaml",
            "config/rbac/leader_election_role_binding.yaml",
            "config/sandbox-manager/serviceaccount.yaml",
            "config/sandbox-manager/rbac.yaml",
            "config/sandbox-gateway/serviceaccount.yaml",
            "config/sandbox-gateway/rbac.yaml",
        ):
            with self.subTest(source=source):
                self.assertIn(source, content)


FAKE_KUSTOMIZE = """#!/usr/bin/env python3
import os
import sys
from pathlib import Path

overlay = Path(sys.argv[2]).name
fixtures = Path(os.environ["FAKE_KUSTOMIZE_FIXTURES"])
sys.stdout.write((fixtures / (overlay + ".yaml")).read_text(encoding="utf-8"))
"""

FAKE_HELM = """#!/usr/bin/env python3
import os
import sys
from pathlib import Path

args = sys.argv[1:]
template = Path(args[args.index("-s") + 1]).name
chart = Path(args[2]).parent.name
fixtures = Path(os.environ["FAKE_HELM_FIXTURES"])
sys.stdout.write((fixtures / chart / template).read_text(encoding="utf-8"))
"""

CHART_LABELS = {
    "helm.sh/chart": "sandbox-0.1.0",
    "app.kubernetes.io/managed-by": "Helm",
    "app.kubernetes.io/instance": "sandbox",
    "app.kubernetes.io/version": "0.1.0",
}

CONTROLLER_CHART_NAME = "kruise-agents-sandbox-controller"
MANAGER_CHART_NAME = "kruise-agents-sandbox-manager"


def controller_source_docs() -> list[dict]:
    return [
        {
            "apiVersion": "v1",
            "kind": "Namespace",
            "metadata": {"name": "sandbox-system"},
        },
        {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": "configuration"},
            "data": {"controller_manager_env.yaml": "ENABLE_WEBHOOKS: true"},
        },
        {
            "apiVersion": "v1",
            "kind": "ServiceAccount",
            "metadata": {"name": "controller-manager"},
        },
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {
                "name": "controller-manager-webhook-service",
                "labels": {"control-plane": "controller-manager"},
            },
            "spec": {
                "type": "ClusterIP",
                "ports": [
                    {"port": 443, "targetPort": 9443, "protocol": "TCP", "name": "webhook"}
                ],
            },
        },
    ]


def manager_source_docs() -> list[dict]:
    return [
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {"name": "sandbox-manager"},
            "spec": {
                "type": "ClusterIP",
                "ports": [{"port": 7788, "targetPort": 7788, "name": "http-envoy"}],
            },
        },
        {
            "apiVersion": "networking.k8s.io/v1",
            "kind": "Ingress",
            "metadata": {"name": "sandbox-manager"},
            "spec": {
                "ingressClassName": "nginx",
                "tls": [{"hosts": ["example.com"], "secretName": "sandbox-manager-tls"}],
                "rules": [
                    {
                        "host": "example.com",
                        "http": {
                            "paths": [
                                {
                                    "path": "/",
                                    "pathType": "Prefix",
                                    "backend": {
                                        "service": {
                                            "name": "sandbox-manager",
                                            "port": {"number": 7788},
                                        }
                                    },
                                }
                            ]
                        },
                    }
                ],
            },
        },
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": "e2b-key-store"},
            "type": "Opaque",
            "data": {},
        },
        {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": "sandbox-manager-envoy-config"},
            "data": {
                "envoy.yaml": (
                    "admin:\n"
                    "  address:\n"
                    "    socket_address:\n"
                    "      port_value: 9901\n"
                )
            },
        },
    ]


def gateway_source_docs() -> list[dict]:
    return [
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {"name": "sandbox-gateway"},
            "spec": {
                "type": "ClusterIP",
                "ports": [{"port": 80, "targetPort": 8081, "name": "http"}],
            },
        },
        {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": "envoy-config"},
            "data": {
                "envoy.yaml": "static_resources:\n  listeners:\n    - name: http\n"
            },
        },
    ]


def default_source_docs() -> dict[str, list[dict]]:
    return {
        "manager": controller_source_docs(),
        "sandbox-manager": manager_source_docs(),
        "sandbox-gateway": gateway_source_docs(),
    }


def chart_doc(source_doc: dict) -> dict:
    doc = copy.deepcopy(source_doc)
    metadata = dict(doc.get("metadata") or {})
    metadata["labels"] = {**(metadata.get("labels") or {}), **CHART_LABELS}
    if doc.get("kind") == "Secret":
        metadata["annotations"] = {
            **(metadata.get("annotations") or {}),
            "helm.sh/resource-policy": "keep",
        }
    doc["metadata"] = metadata
    return doc


def default_chart_docs() -> dict[tuple[str, str], list[dict]]:
    controller = {doc["kind"]: doc for doc in controller_source_docs()}
    manager = {doc["kind"]: doc for doc in manager_source_docs()}
    gateway = {doc["kind"]: doc for doc in gateway_source_docs()}
    return {
        (CONTROLLER_CHART_NAME, "service.yaml"): [chart_doc(controller["Service"])],
        (MANAGER_CHART_NAME, "service.yaml"): [
            chart_doc(manager["Service"]),
            chart_doc(gateway["Service"]),
        ],
        (MANAGER_CHART_NAME, "ingress.yaml"): [chart_doc(manager["Ingress"])],
        (MANAGER_CHART_NAME, "secret.yaml"): [chart_doc(manager["Secret"])],
        (MANAGER_CHART_NAME, "envoy-config.yaml"): [chart_doc(manager["ConfigMap"])],
        (MANAGER_CHART_NAME, "gateway-envoy-config.yaml"): [chart_doc(gateway["ConfigMap"])],
    }


def dump_docs(docs: list[dict]) -> str:
    return yaml.safe_dump_all(docs, sort_keys=False, default_flow_style=False)


class ManifestDriftTest(unittest.TestCase):
    def setUp(self) -> None:
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.agents_repo = self.root / "agents"
        self.charts_repo = self.root / "charts"

    def write_source_fixtures(self, overlay_docs: dict[str, list[dict]]) -> None:
        fixtures = self.agents_repo / ".kustomize-fixtures"
        for overlay, docs in overlay_docs.items():
            fixtures.mkdir(parents=True, exist_ok=True)
            (fixtures / f"{overlay}.yaml").write_text(dump_docs(docs), encoding="utf-8")
        kustomize = self.agents_repo / "bin" / "kustomize"
        kustomize.parent.mkdir(parents=True, exist_ok=True)
        kustomize.write_text(FAKE_KUSTOMIZE, encoding="utf-8")
        kustomize.chmod(0o755)

    def write_chart_fixtures(
        self,
        chart_docs: dict[tuple[str, str], list[dict]],
        template_sources: dict[tuple[str, str], str] | None = None,
    ) -> None:
        for (chart, template), docs in chart_docs.items():
            fixture = self.root / ".helm-fixtures" / chart / template
            fixture.parent.mkdir(parents=True, exist_ok=True)
            fixture.write_text(dump_docs(docs), encoding="utf-8")
            template_file = self.charts_repo / "versions" / chart / "next" / "templates" / template
            template_file.parent.mkdir(parents=True, exist_ok=True)
            content = (template_sources or {}).get((chart, template), "# chart template source\n")
            template_file.write_text(content, encoding="utf-8")
        helm = self.root / "fakebin" / "helm"
        helm.parent.mkdir(parents=True, exist_ok=True)
        helm.write_text(FAKE_HELM, encoding="utf-8")
        helm.chmod(0o755)

    def run_checker(self, *args: str) -> subprocess.CompletedProcess:
        env = {
            **os.environ,
            "PATH": f"{self.root / 'fakebin'}{os.pathsep}{os.environ['PATH']}",
            "FAKE_KUSTOMIZE_FIXTURES": str(self.agents_repo / ".kustomize-fixtures"),
            "FAKE_HELM_FIXTURES": str(self.root / ".helm-fixtures"),
        }
        return subprocess.run(
            [
                sys.executable,
                str(CHECKER),
                "--agents-repo",
                str(self.agents_repo),
                "--charts-repo",
                str(self.charts_repo),
                *args,
            ],
            check=False,
            capture_output=True,
            text=True,
            env=env,
        )

    def run_manifests_checker(self, *extra_args: str) -> subprocess.CompletedProcess:
        return self.run_checker("--aspect", "manifests", *extra_args)

    def test_reports_every_mapped_manifest_clean_ignoring_skipped_documents(self) -> None:
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(default_chart_docs())

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 0, result.stderr)
        for line in (
            "OK manifests controller Service/controller-manager-webhook-service",
            "OK manifests manager Service/sandbox-manager",
            "OK manifests manager Ingress/sandbox-manager",
            "OK manifests manager Secret/e2b-key-store",
            "OK manifests manager ConfigMap/sandbox-manager-envoy-config",
            "OK manifests gateway Service/sandbox-gateway",
            "OK manifests gateway ConfigMap/envoy-config",
        ):
            with self.subTest(line=line):
                self.assertIn(line, result.stdout)
        self.assertNotIn("UNMAPPED", result.stdout)
        self.assertNotIn("Namespace", result.stdout)
        self.assertNotIn("configuration", result.stdout)

    def test_reports_missing_chart_template(self) -> None:
        chart_docs = default_chart_docs()
        del chart_docs[(MANAGER_CHART_NAME, "secret.yaml")]
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(chart_docs)

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(
            "MISSING manifests manager Secret/e2b-key-store: templates/secret.yaml not found",
            result.stdout,
        )

    def test_reports_unmapped_source_manifest(self) -> None:
        source_docs = default_source_docs()
        source_docs["sandbox-manager"].append(
            {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "sandbox-manager-extra"}}
        )
        self.write_source_fixtures(source_docs)
        self.write_chart_fixtures(default_chart_docs())

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertIn("UNMAPPED manifests manager ConfigMap/sandbox-manager-extra", result.stdout)

    def test_reports_chart_only_port_as_helm_only(self) -> None:
        chart_docs = default_chart_docs()
        manager_service = chart_docs[(MANAGER_CHART_NAME, "service.yaml")][0]
        manager_service["spec"]["ports"].append(
            {"port": 9002, "targetPort": 9002, "name": "grpc-extproc", "protocol": "TCP"}
        )
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(chart_docs)

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(
            "HELM_ONLY manifests manager Service/sandbox-manager: spec.ports[1] chart-only port 9002",
            result.stdout,
        )
        self.assertNotIn("DRIFT manifests", result.stdout)

    def test_marks_value_difference_on_templated_field_as_templated(self) -> None:
        chart_docs = default_chart_docs()
        manager_envoy = chart_docs[(MANAGER_CHART_NAME, "envoy-config.yaml")][0]
        manager_envoy["data"]["envoy.yaml"] = (
            "admin:\n"
            "  address:\n"
            "    socket_address:\n"
            "      port_value: 9902\n"
        )
        template_sources = {
            (
                MANAGER_CHART_NAME,
                "envoy-config.yaml",
            ): "admin:\n  address:\n    socket_address:\n      port_value: {{ .Values.envoy.adminPort }}\n"
        }
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(chart_docs, template_sources)

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(
            "TEMPLATED manifests manager ConfigMap/sandbox-manager-envoy-config: "
            "data.envoy.yaml.admin.address.socket_address.port_value source 9901 != chart 9902",
            result.stdout,
        )
        self.assertIn("chart renders this field through a template", result.stdout)
        self.assertNotIn("DRIFT manifests", result.stdout)

    def test_keeps_plain_drift_when_template_is_literal(self) -> None:
        chart_docs = default_chart_docs()
        manager_envoy = chart_docs[(MANAGER_CHART_NAME, "envoy-config.yaml")][0]
        manager_envoy["data"]["envoy.yaml"] = (
            "admin:\n"
            "  address:\n"
            "    socket_address:\n"
            "      port_value: 9902\n"
        )
        template_sources = {
            (
                MANAGER_CHART_NAME,
                "envoy-config.yaml",
            ): "admin:\n  address:\n    socket_address:\n      port_value: 9902\n"
        }
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(chart_docs, template_sources)

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(
            "DRIFT manifests manager ConfigMap/sandbox-manager-envoy-config: "
            "data.envoy.yaml.admin.address.socket_address.port_value source 9901 != chart 9902",
            result.stdout,
        )
        self.assertNotIn("TEMPLATED", result.stdout)
        self.assertNotIn("chart renders this field through a template", result.stdout)

    def test_keeps_missing_field_as_drift_even_when_templated(self) -> None:
        chart_docs = default_chart_docs()
        manager_envoy = chart_docs[(MANAGER_CHART_NAME, "envoy-config.yaml")][0]
        manager_envoy["data"]["envoy.yaml"] = "admin:\n  address:\n    socket_address: {}\n"
        template_sources = {
            (
                MANAGER_CHART_NAME,
                "envoy-config.yaml",
            ): "admin:\n  address:\n    socket_address:\n      port_value: {{ .Values.envoy.adminPort }}\n"
        }
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(chart_docs, template_sources)

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(
            "DRIFT manifests manager ConfigMap/sandbox-manager-envoy-config: "
            "data.envoy.yaml.admin.address.socket_address.port_value missing in chart",
            result.stdout,
        )
        self.assertNotIn("TEMPLATED", result.stdout)

    def test_marks_range_templated_list_difference_as_templated(self) -> None:
        source_docs = default_source_docs()
        gateway_envoy = next(
            doc
            for doc in source_docs["sandbox-gateway"]
            if doc.get("metadata", {}).get("name") == "envoy-config"
        )
        gateway_envoy["data"]["envoy.yaml"] = (
            "static_resources:\n"
            "  listeners:\n"
            "    - name: http\n"
            "  thresholds:\n"
            "    - max_connections: 80000\n"
            "      priority: DEFAULT\n"
            "    - max_connections: 100\n"
            "      priority: HIGH\n"
        )
        chart_docs = default_chart_docs()
        chart_gateway_envoy = chart_docs[(MANAGER_CHART_NAME, "gateway-envoy-config.yaml")][0]
        chart_gateway_envoy["data"]["envoy.yaml"] = (
            "static_resources:\n"
            "  listeners:\n"
            "    - name: http\n"
            "  thresholds:\n"
            "    - max_connections: 80000\n"
            "      priority: DEFAULT\n"
        )
        template_sources = {
            (
                MANAGER_CHART_NAME,
                "gateway-envoy-config.yaml",
            ): (
                "static_resources:\n"
                "  listeners:\n"
                "    - name: http\n"
                "  thresholds:\n"
                "{{- range .Values.gateway.envoy.circuitBreakers.thresholds }}\n"
                "    - max_connections: {{ .maxConnections }}\n"
                "      priority: {{ .priority }}\n"
                "{{- end }}\n"
            )
        }
        self.write_source_fixtures(source_docs)
        self.write_chart_fixtures(chart_docs, template_sources)

        result = self.run_manifests_checker()

        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn(
            "TEMPLATED manifests gateway ConfigMap/envoy-config: "
            "data.envoy.yaml.static_resources.thresholds source item",
            result.stdout,
        )
        self.assertIn("missing in chart; chart renders this field through a template", result.stdout)
        self.assertNotIn("DRIFT manifests", result.stdout)

    def test_component_gateway_checks_only_gateway(self) -> None:
        self.write_source_fixtures(default_source_docs())
        self.write_chart_fixtures(default_chart_docs())

        result = self.run_manifests_checker("--component", "gateway")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("OK manifests gateway Service/sandbox-gateway", result.stdout)
        self.assertIn("OK manifests gateway ConfigMap/envoy-config", result.stdout)
        self.assertNotIn(" manifests controller ", result.stdout)
        self.assertNotIn(" manifests manager ", result.stdout)

    def test_rejects_invalid_aspect_flag_combinations(self) -> None:
        for extra_args, message in (
            (
                ("--aspect", "manifests", "--apply-crds"),
                "--apply-crds is only valid with --aspect crd",
            ),
            (
                ("--aspect", "crd", "--kinds", "Service"),
                "--kinds is only valid with --aspect manifests",
            ),
            (
                ("--aspect", "crd", "--component", "gateway"),
                "--component gateway is only valid with --aspect manifests",
            ),
        ):
            with self.subTest(extra_args=extra_args):
                result = self.run_checker(*extra_args)

                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn(message, result.stderr)

    def test_documents_deployment_manifest_synchronization(self) -> None:
        content = SKILL.read_text(encoding="utf-8")

        self.assertIn("## Deployment Manifests", content)
        for requirement in (
            "--aspect manifests --component all",
            "--aspect manifests --component gateway",
            "--aspect manifests --kinds Service,ConfigMap",
            "controller-manager-webhook-service",
            "sandbox-manager-envoy-config",
            "e2b-key-store",
            "envoy-config",
            "templates/gateway-envoy-config.yaml",
            "(document 1)",
            "config/manager",
            "config/sandbox-manager",
            "config/sandbox-gateway",
            "read-only manifests checker",
            "`HELM_ONLY` — chart-only content",
            "`MISSING` — a mapped chart template file does not exist",
            "`UNMAPPED` — a source manifest of a managed kind",
            "`TEMPLATED` — a value difference on a field the chart renders",
            "`0` clean, `1` drift (including `TEMPLATED`) or missing",
            "Deployments are not synchronized by this checker",
            "`MANIFEST_SPEC` mapping",
            "Preserve chart-managed metadata",
            "fix values-driven drift by updating `values.yaml` defaults first",
            "Match source order for indexed lists",
            "template regressions",
        ):
            with self.subTest(requirement=requirement):
                self.assertIn(requirement, content)


if __name__ == "__main__":
    unittest.main()
