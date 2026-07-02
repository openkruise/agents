# `pkg/utils/lifecycle` Guide

This package contains shared Sandbox CRD lifecycle predicates used across cache,
infra, and controller code.

## Boundaries

- Keep predicates pure and deterministic: no clients, informers, Redis, backend calls, manager calls, or HTTP/E2B semantics.
- It may depend on `api/v1alpha1` CRD types, but must not import `pkg/sandbox-manager`, `pkg/cache`, `pkg/servers`, or backend packages.
- Only add predicates that are genuinely shared across multiple domains. If a lifecycle rule is specific to quota storage, HTTP behavior, or one controller, keep it local there.
- Keep names tied to Sandbox lifecycle meaning, not to a particular backend implementation.
