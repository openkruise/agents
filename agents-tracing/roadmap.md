High-level overview of our main strategic priorities for 2026:

* pool management
    * Inplace Resizing Support
    * Autoscaling Support
    * Template rolling update support

* Storage
    * Dynamic storage mounting support for OSS storage options
    * Avoid manual configuration of CSI sidecars

* Network
    * Sandbox gateway performance benchmarking and enhancement
    * Lightweight network access control

* Runtime
    * Add Agent-runtime that is compatible with E2B envd but runs inside a sidecar
    * Experimental support for pause/checkpoint with filesystem persistency
    * Kata/gvisor/kuasar best practice
    * Security hardening for agent runtime such as more secure access token and auditing

* Schedulering
    * Integrate with fast scheduler feature in Koorindator and Volcano

* API
    * Add missing E2B API support(network access control, signed file download, team api etc. )
    * Complete K8S API support for agent-runtime services e.g. command, file transfer
    * Publish java and python sdk to PyPi and Maven for easier installation

* observability
    * More metrics for control-plane
    * Tracing support
    * Benchmark guidelines

* Integration
    * Supporting feature and best practices for OpenClaw
    * Best practice to run with agentic-RL framework such as verl, roll
    * Supporting feature and best practices for more desktop-uses and  mobile uses
    * Supporting feature and best practices for RL benchmark such as swe-bench
