# vm-manager

Prototype. An MCP + REST server that provisions cloud-provider-like VMs (IMDS, vTPM with
measured boot, immutable mkosi-built OS, Kubernetes as a sysext layer) on a KVM host, so
agents on the Giant Swarm agent platform can hand them to the CAPI based cluster-manager.

Sibling of [agent-manager](https://github.com/giantswarm/agent-manager) and
[model-manager](https://github.com/giantswarm/model-manager).

- [Design](docs/design.md)
- [Roadmap and agent plan](docs/plan.md)
