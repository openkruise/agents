#!/usr/bin/env python3
"""Check CRD and deployment-manifest drift between config/ and the charts checkout."""

from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

import yaml


CONTROLLER_OVERLAY = Path("config/manager")
MANAGER_OVERLAY = Path("config/sandbox-manager")
GATEWAY_OVERLAY = Path("config/sandbox-gateway")

CONTROLLER_CHART = "kruise-agents-sandbox-controller"
MANAGER_CHART = "kruise-agents-sandbox-manager"

CHART_NAMESPACE = "sandbox-system"


@dataclass(frozen=True)
class Target:
    component: str
    path: Path


CHART_SPEC = {
    "agents.kruise.io_checkpoints.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_checkpoints.yaml"),
    ),
    "agents.kruise.io_commits.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_commits.yaml"),
    ),
    "agents.kruise.io_poolautoscalers.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_poolautoscalers.yaml"),
    ),
    "agents.kruise.io_sandboxclaims.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_sandboxclaims.yaml"),
    ),
    "agents.kruise.io_sandboxes.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_sandboxes.yaml"),
    ),
    "agents.kruise.io_sandboxsets.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_sandboxsets.yaml"),
    ),
    "agents.kruise.io_sandboxtemplates.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_sandboxtemplates.yaml"),
    ),
    "agents.kruise.io_sandboxupdateops.yaml": Target(
        "controller",
        Path("versions/kruise-agents-sandbox-controller/next/crds/agents.kruise.io_sandboxupdateops.yaml"),
    ),
    "agents.kruise.io_trafficpolicies.yaml": Target(
        "manager",
        Path("versions/kruise-agents-sandbox-manager/next/files/agentio/trafficpolicy-crd.yaml"),
    ),
    "agents.kruise.io_globaltrafficpolicies.yaml": Target(
        "manager",
        Path("versions/kruise-agents-sandbox-manager/next/files/agentio/globaltrafficpolicy-crd.yaml"),
    ),
    "agents.kruise.io_securityprofiles.yaml": Target(
        "manager",
        Path("versions/kruise-agents-sandbox-manager/next/files/agentio/securityprofile-crd.yaml"),
    ),
    "agents.kruise.io_globalsecurityprofiles.yaml": Target(
        "manager",
        Path("versions/kruise-agents-sandbox-manager/next/files/agentio/globalsecurityprofile-crd.yaml"),
    ),
}


@dataclass(frozen=True)
class ManifestTarget:
    component: str
    overlay: Path
    kind: str
    name: str
    chart: str
    template: Path
    doc_index: int


MANIFEST_SPEC = (
    ManifestTarget("controller", CONTROLLER_OVERLAY, "Service", "controller-manager-webhook-service", CONTROLLER_CHART, Path("templates/service.yaml"), 0),
    ManifestTarget("manager", MANAGER_OVERLAY, "Service", "sandbox-manager", MANAGER_CHART, Path("templates/service.yaml"), 0),
    ManifestTarget("manager", MANAGER_OVERLAY, "Ingress", "sandbox-manager", MANAGER_CHART, Path("templates/ingress.yaml"), 0),
    ManifestTarget("manager", MANAGER_OVERLAY, "Secret", "e2b-key-store", MANAGER_CHART, Path("templates/secret.yaml"), 0),
    ManifestTarget("manager", MANAGER_OVERLAY, "ConfigMap", "sandbox-manager-envoy-config", MANAGER_CHART, Path("templates/envoy-config.yaml"), 0),
    ManifestTarget("gateway", GATEWAY_OVERLAY, "Service", "sandbox-gateway", MANAGER_CHART, Path("templates/service.yaml"), 1),
    ManifestTarget("gateway", GATEWAY_OVERLAY, "ConfigMap", "envoy-config", MANAGER_CHART, Path("templates/gateway-envoy-config.yaml"), 0),
)

OVERLAY_COMPONENTS = {
    CONTROLLER_OVERLAY: "controller",
    MANAGER_OVERLAY: "manager",
    GATEWAY_OVERLAY: "gateway",
}

MANAGED_KINDS = frozenset({"Service", "ConfigMap", "Ingress", "Secret"})
EXCLUDED_SOURCES = frozenset({("controller", "ConfigMap", "configuration")})

CHART_RELEASES = {
    CONTROLLER_CHART: ("sandbox-controller", ()),
    MANAGER_CHART: ("sandbox-manager", ("ingress.className=nginx", "e2b.adminApiKey=x")),
}

CHART_MANAGED_LABELS = frozenset(
    {
        "helm.sh/chart",
        "app.kubernetes.io/managed-by",
        "app.kubernetes.io/instance",
        "app.kubernetes.io/version",
        "app.kubernetes.io/part-of",
        "app.kubernetes.io/component",
    }
)
CHART_MANAGED_ANNOTATIONS = frozenset({"helm.sh/resource-policy"})


class ConfigurationError(Exception):
    pass


def resources(kustomization: Path) -> list[Path]:
    if not kustomization.is_file():
        raise ConfigurationError(f"missing kustomization: {kustomization}")

    result: list[Path] = []
    active = False
    for line in kustomization.read_text(encoding="utf-8").splitlines():
        if not active:
            active = line.strip() == "resources:"
            continue
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if re.match(r"^[A-Za-z][A-Za-z0-9_-]*:\s*", line):
            break
        match = re.match(r"^\s*-\s+([^#]+?)(?:\s+#.*)?$", line)
        if match:
            result.append(Path(match.group(1).strip().strip("'\"")))

    if not active:
        raise ConfigurationError(f"resources key not found: {kustomization}")
    return result


def synchronize(args: argparse.Namespace) -> int:
    if args.component == "gateway":
        raise ConfigurationError("--component gateway is only valid with --aspect manifests")
    if not args.charts_repo.is_dir():
        raise ConfigurationError(f"missing charts checkout: {args.charts_repo}")
    if not (args.charts_repo / "versions").is_dir():
        raise ConfigurationError(f"missing charts versions directory: {args.charts_repo / 'versions'}")

    crd_dir = args.agents_repo / "config" / "crd"
    entries: list[tuple[Path, Target]] = []
    has_unmapped = False

    for resource in resources(crd_dir / "kustomization.yaml"):
        target = CHART_SPEC.get(resource.name)
        if target is None:
            print(f"UNMAPPED crd {resource.name}")
            has_unmapped = True
            continue
        if args.component != "all" and args.component != target.component:
            continue
        source = crd_dir / resource
        if not source.is_file():
            raise ConfigurationError(f"missing CRD source: {source}")
        entries.append((source, target))

    if args.apply_crds and has_unmapped:
        return 3

    if args.apply_crds:
        for source, target in entries:
            destination = args.charts_repo / target.path
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, destination)
            print(f"SYNCED crd {target.component} {target.path}")

    has_drift = False
    for source, target in entries:
        destination = args.charts_repo / target.path
        if not destination.is_file():
            print(f"DRIFT crd {target.component} {target.path} missing")
            has_drift = True
        elif source.read_bytes() != destination.read_bytes():
            print(f"DRIFT crd {target.component} {target.path} content")
            has_drift = True
        else:
            print(f"OK crd {target.component} {target.path}")
    if has_unmapped:
        return 3
    return 1 if has_drift else 0


def run_command(command: list[str], cwd: Path) -> str:
    try:
        result = subprocess.run(command, cwd=cwd, capture_output=True, text=True)
    except OSError as error:
        raise ConfigurationError(f"cannot execute {command[0]}: {error}") from error
    if result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip().splitlines()
        raise ConfigurationError(f"command failed: {' '.join(command)}: {detail[0] if detail else 'unknown error'}")
    return result.stdout


def parse_docs(text: str) -> list[dict]:
    return [doc for doc in yaml.safe_load_all(text) if isinstance(doc, dict)]


def render_source(agents_repo: Path, overlay: Path) -> list[dict]:
    kustomize = agents_repo / "bin" / "kustomize"
    if kustomize.is_file():
        command = [str(kustomize), "build", str(overlay)]
    else:
        command = ["kubectl", "kustomize", str(overlay)]
    return parse_docs(run_command(command, agents_repo))


def render_chart(charts_repo: Path, chart: str, template: Path) -> list[dict]:
    release, settings = CHART_RELEASES[chart]
    chart_dir = charts_repo / "versions" / chart / "next"
    command = ["helm", "template", release, str(chart_dir), "--namespace", CHART_NAMESPACE, "-s", str(template)]
    for setting in settings:
        command += ["--set", setting]
    return parse_docs(run_command(command, charts_repo))


def check_manifests(args: argparse.Namespace) -> int:
    agents_repo = args.agents_repo.resolve()
    charts_repo = args.charts_repo.resolve()
    if not charts_repo.is_dir():
        raise ConfigurationError(f"missing charts checkout: {charts_repo}")
    for chart in (CONTROLLER_CHART, MANAGER_CHART):
        chart_dir = charts_repo / "versions" / chart / "next"
        if not chart_dir.is_dir():
            raise ConfigurationError(f"missing chart directory: {chart_dir}")

    components = {"controller", "manager", "gateway"} if args.component == "all" else {args.component}
    kinds = args.kinds
    targets = [
        target
        for target in MANIFEST_SPEC
        if target.component in components and (kinds is None or target.kind in kinds)
    ]

    source_docs: dict[Path, dict[tuple[str, str], dict]] = {}
    overlays = {target.overlay for target in MANIFEST_SPEC if OVERLAY_COMPONENTS[target.overlay] in components}
    for overlay in overlays:
        docs = render_source(agents_repo, overlay)
        source_docs[overlay] = {(doc.get("kind"), (doc.get("metadata") or {}).get("name")): doc for doc in docs}

    has_drift = False
    has_unmapped = False
    chart_renders: dict[tuple[str, Path], list[dict]] = {}
    template_texts: dict[tuple[str, Path], str] = {}

    for target in targets:
        source = source_docs[target.overlay].get((target.kind, target.name))
        if source is None:
            print(f"DRIFT manifests {target.component} {target.kind}/{target.name}: not found in rendered source {target.overlay}")
            has_drift = True
            continue
        if not (charts_repo / "versions" / target.chart / "next" / target.template).is_file():
            print(f"MISSING manifests {target.component} {target.kind}/{target.name}: {target.template} not found")
            has_drift = True
            continue
        key = (target.chart, target.template)
        if key not in chart_renders:
            chart_renders[key] = render_chart(charts_repo, target.chart, target.template)
        docs = chart_renders[key]
        if target.doc_index >= len(docs):
            print(
                f"DRIFT manifests {target.component} {target.kind}/{target.name}: "
                f"{target.template} renders {len(docs)} document(s), expected index {target.doc_index}"
            )
            has_drift = True
            continue
        chart = docs[target.doc_index]
        if chart.get("kind") != target.kind:
            print(
                f"DRIFT manifests {target.component} {target.kind}/{target.name}: "
                f"{target.template}[{target.doc_index}] renders {chart.get('kind')}"
            )
            has_drift = True
            continue
        findings = compare_manifest(source, chart)
        if key not in template_texts:
            template_texts[key] = (charts_repo / "versions" / target.chart / "next" / target.template).read_text(
                encoding="utf-8"
            )
        mark_templated(findings, template_texts[key])
        if any(finding.category in ("DRIFT", "TEMPLATED") for finding in findings):
            has_drift = True
        if findings:
            for finding in findings:
                detail = shorten(f"{finding.path} {finding.detail}")
                print(f"{finding.category} manifests {target.component} {target.kind}/{target.name}: {detail}")
        else:
            print(f"OK manifests {target.component} {target.kind}/{target.name}")

    for component in sorted(components):
        overlay = next(key for key, value in OVERLAY_COMPONENTS.items() if value == component)
        for kind, name in source_docs[overlay]:
            if kinds is not None and kind not in kinds:
                continue
            if kind not in MANAGED_KINDS:
                continue
            if (component, kind, name) in EXCLUDED_SOURCES:
                continue
            if not any(
                target.component == component and target.kind == kind and target.name == name
                for target in MANIFEST_SPEC
            ):
                print(f"UNMAPPED manifests {component} {kind}/{name}")
                has_unmapped = True

    if has_unmapped:
        return 3
    return 1 if has_drift else 0


def shorten(detail: str, limit: int = 200) -> str:
    detail = " ".join(detail.split())
    return detail if len(detail) <= limit else detail[: limit - 3] + "..."


@dataclass
class Finding:
    category: str
    path: str
    detail: str


def compare_manifest(source: dict, chart: dict) -> list[Finding]:
    findings: list[Finding] = []
    compare_metadata(source, chart, findings)
    comparators = {
        "Service": compare_service,
        "ConfigMap": compare_configmap,
        "Ingress": compare_ingress,
        "Secret": compare_secret,
    }
    comparators[source.get("kind", "")](source, chart, findings)
    return findings


def leaf_key(path: str) -> str:
    key = path.rsplit(".", 1)[-1]
    return key.split("[", 1)[0].strip()


def is_value_difference(detail: str) -> bool:
    return " != chart " in detail or detail == "source and chart differ" or detail.startswith("source item ")


def template_renders_key(template_text: str, key: str) -> bool:
    direct = re.search(r"^[ \t]*" + re.escape(key) + r"[ \t]*:[^\n]*\{\{", template_text, re.MULTILINE)
    if direct:
        return True
    return re.search(r"\{\{-?[ \t]*range[^\n]*\." + re.escape(key) + r"[ \t]*-?\}\}", template_text) is not None


def mark_templated(findings: list[Finding], template_text: str) -> None:
    for finding in findings:
        if finding.category != "DRIFT" or not is_value_difference(finding.detail):
            continue
        if not template_renders_key(template_text, leaf_key(finding.path)):
            continue
        finding.category = "TEMPLATED"
        finding.detail += "; chart renders this field through a template"


def compare_metadata(source: dict, chart: dict, findings: list[Finding]) -> None:
    source_meta = source.get("metadata") or {}
    chart_meta = chart.get("metadata") or {}
    for field, managed in (("labels", CHART_MANAGED_LABELS), ("annotations", CHART_MANAGED_ANNOTATIONS)):
        source_map = {key: value for key, value in (source_meta.get(field) or {}).items() if key not in managed}
        chart_map = {key: value for key, value in (chart_meta.get(field) or {}).items() if key not in managed}
        compare_mapping(source_map, chart_map, f"metadata.{field}", findings)


def compare_mapping(source_map: dict, chart_map: dict, path: str, findings: list[Finding]) -> None:
    for key, value in source_map.items():
        if key not in chart_map:
            findings.append(Finding("DRIFT", f"{path}.{key}", "missing in chart"))
        elif chart_map[key] != value:
            findings.append(Finding("DRIFT", f"{path}.{key}", f"source {value!r} != chart {chart_map[key]!r}"))
    for key in chart_map:
        if key not in source_map:
            findings.append(Finding("HELM_ONLY", f"{path}.{key}", "chart-only"))


def compare_structured(source, chart, path: str, findings: list[Finding], helm_only_keys: frozenset[str] = frozenset()) -> None:
    if isinstance(source, dict) and isinstance(chart, dict):
        for key, value in source.items():
            if key in helm_only_keys:
                if key not in chart or chart[key] != value:
                    findings.append(Finding("HELM_ONLY", f"{path}.{key}", "helm-managed field differs"))
                continue
            if key not in chart:
                findings.append(Finding("DRIFT", f"{path}.{key}", "missing in chart"))
            else:
                compare_structured(value, chart[key], f"{path}.{key}", findings, helm_only_keys)
        for key in chart:
            if key not in source:
                if key in helm_only_keys:
                    findings.append(Finding("HELM_ONLY", f"{path}.{key}", "chart-only, helm-managed"))
                else:
                    findings.append(Finding("HELM_ONLY", f"{path}.{key}", "chart-only"))
    elif isinstance(source, list) and isinstance(chart, list):
        source_items = _named_items(source)
        chart_items = _named_items(chart)
        if source_items is not None and chart_items is not None:
            for key, value in source_items.items():
                if key not in chart_items:
                    findings.append(Finding("DRIFT", f"{path}[name={key}]", "missing in chart"))
                else:
                    compare_structured(value, chart_items[key], f"{path}[name={key}]", findings, helm_only_keys)
            for key in chart_items:
                if key not in source_items:
                    findings.append(Finding("HELM_ONLY", f"{path}[name={key}]", "chart-only"))
        else:
            source_values = [_canonical(item) for item in source]
            chart_values = [_canonical(item) for item in chart]
            for value in source_values:
                if value not in chart_values:
                    findings.append(Finding("DRIFT", path, f"source item {value} missing in chart"))
            for value in chart_values:
                if value not in source_values:
                    findings.append(Finding("HELM_ONLY", path, f"chart-only item {value}"))
    elif source != chart:
        findings.append(Finding("DRIFT", path, f"source {source!r} != chart {chart!r}"))


def _named_items(items: list) -> dict | None:
    if not items or not all(isinstance(item, dict) and "name" in item for item in items):
        return None
    return {item["name"]: item for item in items}


def _canonical(value) -> str:
    return json.dumps(value, sort_keys=True, default=str)


def compare_service(source: dict, chart: dict, findings: list[Finding]) -> None:
    source_spec = source.get("spec") or {}
    chart_spec = chart.get("spec") or {}
    if "type" in source_spec and source_spec.get("type") != chart_spec.get("type"):
        findings.append(
            Finding("DRIFT", "spec.type", f"source {source_spec.get('type')!r} != chart {chart_spec.get('type')!r}")
        )
    source_ports = source_spec.get("ports") or []
    chart_ports = chart_spec.get("ports") or []
    for index, port in enumerate(source_ports):
        if index >= len(chart_ports):
            findings.append(Finding("DRIFT", f"spec.ports[{index}]", "missing in chart"))
            break
        match = chart_ports[index]
        for field in ("port", "targetPort"):
            if port.get(field) is not None and port.get(field) != match.get(field):
                findings.append(
                    Finding(
                        "DRIFT",
                        f"spec.ports[{index}].{field}",
                        f"source {port.get(field)!r} != chart {match.get(field)!r}",
                    )
                )
        source_protocol = port.get("protocol") or "TCP"
        chart_protocol = match.get("protocol") or "TCP"
        if source_protocol != chart_protocol:
            findings.append(
                Finding(
                    "DRIFT",
                    f"spec.ports[{index}].protocol",
                    f"source {source_protocol!r} != chart {chart_protocol!r}",
                )
            )
    for index in range(len(source_ports), len(chart_ports)):
        number = chart_ports[index].get("port")
        findings.append(Finding("HELM_ONLY", f"spec.ports[{index}]", f"chart-only port {number}"))


def compare_configmap(source: dict, chart: dict, findings: list[Finding]) -> None:
    source_data = source.get("data") or {}
    chart_data = chart.get("data") or {}
    for key, value in source_data.items():
        if key not in chart_data:
            findings.append(Finding("DRIFT", f"data.{key}", "missing in chart"))
            continue
        compare_data_value(f"data.{key}", value, chart_data[key], findings)
    for key in chart_data:
        if key not in source_data:
            findings.append(Finding("HELM_ONLY", f"data.{key}", "chart-only key"))


def compare_data_value(path: str, source_value, chart_value, findings: list[Finding]) -> None:
    if isinstance(source_value, str) and isinstance(chart_value, str):
        source_parsed = _maybe_yaml(source_value)
        chart_parsed = _maybe_yaml(chart_value)
        if (
            source_parsed is not None
            and chart_parsed is not None
            and not isinstance(source_parsed, str)
            and not isinstance(chart_parsed, str)
        ):
            compare_structured(source_parsed, chart_parsed, path, findings)
            return
    if source_value != chart_value:
        findings.append(Finding("DRIFT", path, "source and chart differ"))


def _maybe_yaml(text: str):
    try:
        return yaml.safe_load(text)
    except yaml.YAMLError:
        return None


def compare_ingress(source: dict, chart: dict, findings: list[Finding]) -> None:
    source_spec = source.get("spec") or {}
    chart_spec = chart.get("spec") or {}
    if "ingressClassName" in source_spec and source_spec.get("ingressClassName") != chart_spec.get("ingressClassName"):
        findings.append(
            Finding(
                "DRIFT",
                "spec.ingressClassName",
                f"source {source_spec.get('ingressClassName')!r} != chart {chart_spec.get('ingressClassName')!r}",
            )
        )
    source_tls = source_spec.get("tls") or []
    chart_tls = chart_spec.get("tls") or []
    source_secret_names = {entry.get("secretName") for entry in source_tls if isinstance(entry, dict)}
    chart_secret_names = {entry.get("secretName") for entry in chart_tls if isinstance(entry, dict)}
    for secret_name in sorted(name for name in source_secret_names if name is not None):
        if secret_name not in chart_secret_names:
            findings.append(Finding("DRIFT", f"spec.tls[{secret_name}]", "missing in chart"))
    for secret_name in sorted(name for name in chart_secret_names if name is not None):
        if secret_name not in source_secret_names:
            findings.append(Finding("HELM_ONLY", f"spec.tls[{secret_name}]", "chart-only"))

    source_rules = source_spec.get("rules") or []
    chart_rules = chart_spec.get("rules") or []
    for index, rule in enumerate(source_rules):
        if index >= len(chart_rules):
            findings.append(Finding("DRIFT", f"spec.rules[{index}]", "missing in chart"))
            break
        source_paths = (rule.get("http") or {}).get("paths") or []
        chart_paths = (chart_rules[index].get("http") or {}).get("paths") or []
        for path_index, source_path in enumerate(source_paths):
            if path_index >= len(chart_paths):
                findings.append(Finding("DRIFT", f"spec.rules[{index}].http.paths[{path_index}]", "missing in chart"))
                break
            chart_path = chart_paths[path_index]
            for field in ("path", "pathType"):
                if source_path.get(field) is not None and source_path.get(field) != chart_path.get(field):
                    findings.append(
                        Finding(
                            "DRIFT",
                            f"spec.rules[{index}].http.paths[{path_index}].{field}",
                            f"source {source_path.get(field)!r} != chart {chart_path.get(field)!r}",
                        )
                    )
            source_backend = (source_path.get("backend") or {}).get("service") or {}
            chart_backend = (chart_path.get("backend") or {}).get("service") or {}
            source_port = (source_backend.get("port") or {}).get("number")
            chart_port = (chart_backend.get("port") or {}).get("number")
            if source_port is not None and source_port != chart_port:
                findings.append(
                    Finding(
                        "DRIFT",
                        f"spec.rules[{index}].http.paths[{path_index}].backend.service.port.number",
                        f"source {source_port!r} != chart {chart_port!r}",
                    )
                )
            if source_backend.get("name") is not None and source_backend.get("name") != chart_backend.get("name"):
                findings.append(
                    Finding(
                        "DRIFT",
                        f"spec.rules[{index}].http.paths[{path_index}].backend.service.name",
                        f"source {source_backend.get('name')!r} != chart {chart_backend.get('name')!r}",
                    )
                )
        for path_index in range(len(source_paths), len(chart_paths)):
            findings.append(Finding("HELM_ONLY", f"spec.rules[{index}].http.paths[{path_index}]", "chart-only path"))
    if len(chart_rules) > len(source_rules):
        findings.append(
            Finding("HELM_ONLY", f"spec.rules[{len(source_rules)}:]", f"{len(chart_rules) - len(source_rules)} chart-only rule(s)")
        )


def compare_secret(source: dict, chart: dict, findings: list[Finding]) -> None:
    if source.get("type") is not None and source.get("type") != chart.get("type"):
        findings.append(Finding("DRIFT", "type", f"source {source.get('type')!r} != chart {chart.get('type')!r}"))
    source_data = source.get("data") or {}
    chart_data = chart.get("data") or {}
    source_string = source.get("stringData") or {}
    chart_string = chart.get("stringData") or {}
    if not source_data and not source_string:
        if chart_data or chart_string:
            findings.append(Finding("DRIFT", "data", "source secret is empty but chart renders data"))
        return
    for key, value in source_data.items():
        if key not in chart_data:
            findings.append(Finding("DRIFT", f"data.{key}", "missing in chart"))
        elif chart_data[key] != value:
            findings.append(Finding("DRIFT", f"data.{key}", "source and chart differ"))
    for key in chart_data:
        if key not in source_data:
            findings.append(Finding("HELM_ONLY", f"data.{key}", "chart-only key"))
    for key, value in source_string.items():
        if key not in chart_string:
            findings.append(Finding("DRIFT", f"stringData.{key}", "missing in chart"))
        elif chart_string[key] != value:
            findings.append(Finding("DRIFT", f"stringData.{key}", "source and chart differ"))
    for key in chart_string:
        if key not in source_string:
            findings.append(Finding("HELM_ONLY", f"stringData.{key}", "chart-only key"))


def parse_kinds(value: str) -> frozenset[str]:
    kinds = frozenset(kind.strip() for kind in value.split(",") if kind.strip())
    if not kinds:
        raise argparse.ArgumentTypeError("no kinds provided")
    unknown = kinds - MANAGED_KINDS
    if unknown:
        raise argparse.ArgumentTypeError(f"unsupported kinds: {', '.join(sorted(unknown))}")
    return kinds


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agents-repo", type=Path, default=Path.cwd())
    parser.add_argument("--charts-repo", type=Path, required=True)
    parser.add_argument("--component", choices=("all", "controller", "manager", "gateway"), default="all")
    parser.add_argument("--aspect", choices=("crd", "manifests"), default="crd")
    parser.add_argument("--kinds", type=parse_kinds, default=None, help="comma-separated kinds to check (manifests aspect only)")
    parser.add_argument("--target", choices=("next",), default="next")
    parser.add_argument("--apply-crds", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        if args.aspect == "manifests":
            if args.apply_crds:
                raise ConfigurationError("--apply-crds is only valid with --aspect crd")
            return check_manifests(args)
        if args.kinds is not None:
            raise ConfigurationError("--kinds is only valid with --aspect manifests")
        return synchronize(args)
    except ConfigurationError as error:
        print(f"ERROR {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
