# OpenKruise Agents

##  Overview

OpenKruiseAgents provides best practices for managing AI agent workloads in Kubernetes. It is a sub-project of the open source workload project OpenKruise of the Cloud Native Computing Foundation (CNCF) in the AI agent field. OpenKruiseAgent makes AI agents deployment faster, and easy accessible to both AI algorithm scientists and Infra engineers. 

OpenKruiseAgents is aimed to support the following AI agent workloads: 
1. Isolated environment for various tool uses of AI agents
2. Network-accessible and persistent cloud environments for research notebooks and development workspaces
3. Reinforced learning jobs with human-in-the-loop and open-world training
4. Big data training jobs that require quick startup time and strong fault tolerance

## Why OpenKruise Agents

OpenKruise Agents will provide vendor-neutral sandboxes with the following characteristics:

1. Quick and cost-efficient resource provisioning through resource pooling and resource resizing
2. Sandbox hibernation and checkpoint capability including memory, read/write layer data, and GPU memory
3. Management of user identify and sessions and efficient traffic routing without strong dependency on kubernetes services
4. Diverse API and SDK supports that includes both k8s CRD APIs and E2B APIs.

## Relationship with Sig Agent-Sandbox

OpenKruise Agents provides high-level APIs for sandbox management that supports efficient resource provisioning, user management and traffic routing.
Underneath, OpenKruise Agents ship with built-in sandbox API and implementation, and will also support Sig Agent-Sandbox if it is available.
