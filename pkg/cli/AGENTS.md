# okactl CLI

This package implements the user-facing `okactl` command behavior.

## Local Invariants

- Treat command names, aliases, flags, examples, stdout, stderr, and error text
  as user-facing compatibility surfaces.
- Keep Cobra construction thin. Cluster operations belong in functions that
  accept typed client interfaces so unit tests do not need a real cluster.
- Use the narrow Kubernetes operation that matches the command: patch isolated
  fields, validate before update, and create the established CRD for delegated
  work.
- Preserve TemplateRef and container-name guards; a successful command must not
  silently update a field the controller will ignore.
- Long-running waits honor context cancellation and print diagnostics without
  turning optional diagnostic failures into successful mutations.
- Update `docs/developer-manuals/okactl.md` when the command contract changes.
