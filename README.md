# OpenKruise Agents

## Overview

OpenKruise Agents provides best practices for managing AI agent workloads in Kubernetes. It is a sub-project of the open-source workload project OpenKruise under the Cloud Native Computing Foundation (CNCF), specifically tailored for the AI agent domain. OpenKruise Agents accelerates AI agent deployment and makes it easily accessible to both AI algorithm scientists and infrastructure engineers.

OpenKruise Agents is designed to support the following AI agent workloads:
1. Isolated environments for diverse tool usage by AI agents
2. Network-accessible and persistent cloud environments for research notebooks and development workspaces
3. Reinforcement learning jobs featuring human-in-the-loop and open-world training
4. Big data training jobs requiring rapid startup times and robust fault tolerance

## Why OpenKruise Agents

OpenKruise Agents delivers vendor-neutral sandboxes with the following key characteristics:

1. Rapid and cost-effective resource provisioning through resource pooling and dynamic resizing
2. Sandbox hibernation and checkpoint capabilities encompassing memory, read/write layer data, and GPU memory
3. User identity and session management with efficient traffic routing, minimizing dependence on Kubernetes services
4. Comprehensive API and SDK support, including both Kubernetes CRD APIs and E2B APIs

## Relationship with Sig Agent-Sandbox

OpenKruise Agents offers high-level APIs for sandbox management, enabling efficient resource provisioning, user management, and traffic routing. Under the hood, OpenKruise Agents includes built-in sandbox APIs and implementations, while maintaining compatibility with Sig Agent-Sandbox when available.

## Usage
To interact with the OpenKruise Agents programmatically, you can use the E2B SDK, which provides a high-level interface for creating and managing sandboxes.

- [Running E2B Code Interpreter Sandbox](examples/code_interpreter/README.md)
- [Running E2B Desktop Sandbox](examples/desktop/README.md)

## Contributing

You are warmly welcome to hack on OpenKruise Agents. We have prepared a detailed guide [CONTRIBUTING.md](CONTRIBUTING.md).

## Community
Active communication channels:

- Slack: [OpenKruise channel](https://kubernetes.slack.com/channels/openkruise) (*English*)
- DingTalk：Search GroupID `23330762` (*Chinese*)
- WeChat: Search User `openkruise` and let the robot invite you (*Chinese*)
- Bi-weekly Community Meeting (APAC, *Chinese*):
    - Thursday 19:30 GMT+8 (Asia/Shanghai), [Calendar](https://calendar.google.com/calendar/u/2?cid=MjdtbDZucXA2bjVpNTFyYTNpazV2dW8ybHNAZ3JvdXAuY2FsZW5kYXIuZ29vZ2xlLmNvbQ)
    - Join Meeting(DingTalk): Search GroupID `23330762` (*Chinese*)
    - [Notes and agenda](https://shimo.im/docs/gXqmeQOYBehZ4vqo)
- Bi-weekly Community Meeting (*English*): TODO
    - [Meeting Link(zoom)](https://us02web.zoom.us/j/87059136652?pwd=NlI4UThFWXVRZkxIU0dtR1NINncrQT09)