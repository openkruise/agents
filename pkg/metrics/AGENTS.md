# Metrics

- Keep only durable operational signals with bounded labels; never label by resource identity or error text.
- Register metric groups explicitly from component entrypoints; never register them from package `init()`.
- Use domain-prefixed recorder APIs and keep business logic out of this package.
- This package only hosts short-Sandbox-ID and sandbox-route metric groups; do not add unrelated controller/quota series here.
- Surface and shape label values must use the exported constants in this package; do not scatter magic strings at call sites.
