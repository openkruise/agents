# Sandbox-manager graceful shutdown

A terminating Pod can still receive control requests while ingress updates propagate.
After SIGTERM or SIGINT the E2B controller continues normal service, including
health checks, authentication, and all dependencies. It starts its existing
shutdown sequence only after a full second of continuous business inactivity
following the signal. There is no application deadline on this wait.

Each business handler counts from admission through return, including synchronous
compensation and response writes. Detached quota cleanup following API key deletion
and a shared traffic-token issuance count until they finish under their original
operation deadlines, even if all callers have left. Register the actual task before
launching it while its parent handler is still active. Request IDs remain tracing
metadata. Health checks, metrics, peer traffic and Envoy ext-proc do not reset the
idle window.

The final idle check and closing admission are atomic with respect to entry.
A handler admitted first completes normally and restarts the quiet window.
A business request reaching the handler after closure receives HTTP 503.
From the start of the drain, business responses carry `Connection: close`, both
for requests admitted after the drain starts and for those rejected after
closure. A request admitted before the drain finishes normally, so an existing
HTTP/1 connection serves at most one more business request admitted during the
drain and then closes, and a client that reached this Pod through a Service is
routed again on reconnection. Health and metrics responses leave connections
untouched.
The existing component Stop methods run after this decision. If the startup
result is available when a termination signal is observed, a successful startup
opens business admission in either mode and that signal starts the normal idle
wait. A failed startup reports its error. If the startup result is still
unavailable, the signal interrupts startup.

Admission takes one mutex per controller around a few integer operations: it
adjusts the active count and reads the sealed flag, with no I/O, allocation, or
blocking inside. Lock cost scales with hold time times acquisition rate, not with
sharing; at control-plane request rates the lock is idle almost always, so
acquisition is usually the uncontended fast path of a couple of atomic
instructions. It serializes only this admission step, while authentication and
backend I/O in each handler stay fully parallel, leaving throughput I/O-bound
rather than lock-bound. Health checks and metrics bypass the lock. A mutex is
required over an atomic counter because the sealed check and increment must be
atomic with the idle-and-closure decision, and the idle wait uses a condition
variable bound to the same lock. Contention and cache-line effects would matter
only at data-plane rates or if the critical section grew to include I/O; neither
applies here.

One second of local inactivity is a heuristic for residual ingress traffic.
A later arrival can still miss this Pod. Kubernetes enforces its own Pod
termination grace period, including preStop time, and may kill the process
before requests finish.
