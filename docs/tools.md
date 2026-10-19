# The tools

Generated from the registered tools (`make tools-doc`); through muster every tool is `x_<server>_<name>`. The server registers every tool in every mode.

| Tool | Kind |
|---|---|
| `create_network` | write |
| `create_vm` | write |
| `delete_network` | write, destructive |
| `delete_vm` | write, destructive |
| `exec_vm` | write, destructive |
| `forward_port` | write |
| `get_host` | read-only |
| `get_image` | read-only |
| `get_info` | read-only |
| `get_network` | read-only |
| `get_vm` | read-only |
| `get_vm_attestation` | read-only |
| `get_vm_console` | read-only |
| `get_vm_metrics` | read-only |
| `list_images` | read-only |
| `list_networks` | read-only |
| `list_vms` | read-only |
| `reboot_vm` | write |
| `start_vm` | write |
| `stop_vm` | write |

## `create_network`

```text
Create a virtual network for VMs with DHCP, DNS and the instance metadata service at 169.254.169.254.

Rootless (gvisor-tap-vsock). Needed before create_vm unless the default network is enough.
```

```json
{
  "properties": {
    "cidr": {
      "description": "IPv4 subnet, /16 to /29, outside 169.254.0.0/16, e.g. 192.168.128.0/24; the gateway is its first address",
      "type": "string"
    },
    "dns_search_domain": {
      "description": "DNS search domain handed to guests over DHCP",
      "type": "string"
    },
    "name": {
      "description": "Network name, a DNS label of at most 63 characters",
      "type": "string"
    }
  },
  "required": [
    "name",
    "cidr"
  ],
  "type": "object"
}
```

## `create_vm`

```text
Create a VM from a catalog image: install it on a fresh disk, boot it, attest it and serve its user_data.

It allocates a disk on the storage provider, a vTPM, a network lease and a vsock id. Call get_host, list_images and create_network (or list_networks) first. wait_for ready (the default) blocks until the guest is up, up to the install and boot timeouts; none returns at once and get_vm follows the progress. A failure returns the VM id in the error: get_vm_console shows why, delete_vm cleans up.
```

```json
{
  "properties": {
    "cpus": {
      "description": "Virtual CPUs, default 2",
      "type": "number"
    },
    "disk_gib": {
      "description": "Disk size in GiB, default 20",
      "type": "number"
    },
    "hostname": {
      "description": "Guest hostname, default: the name",
      "type": "string"
    },
    "image": {
      "description": "Image reference (\"<id>_<version>\" or bare id); default: the catalog's newest image",
      "type": "string"
    },
    "kubernetes_version": {
      "description": "Kubernetes sysext version served as /kubernetes-version; default: the newest the image offers. See get_image",
      "type": "string"
    },
    "memory_mib": {
      "description": "Memory in MiB, default 2048, at least 256",
      "type": "number"
    },
    "metadata": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "String key/value pairs served as /metadata/<key>",
      "properties": {},
      "type": "object"
    },
    "name": {
      "description": "VM name, a DNS label of at most 63 characters; also the default hostname",
      "type": "string"
    },
    "network": {
      "description": "Network to attach to, default \"default\"",
      "type": "string"
    },
    "require_attestation": {
      "description": "Release user-data only after a verified initrd quote, default true; false releases it as soon as the installed boot starts",
      "type": "boolean"
    },
    "ssh_authorized_keys": {
      "description": "Public keys appended to root's authorized keys, after vm-manager's own per-VM key",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "user_data": {
      "description": "CAPI bootstrap data as Ignition JSON, served at /user-data by the IMDS only after attestation; omit to boot the bare OS",
      "type": "string"
    },
    "wait_for": {
      "description": "Milestone to block on: none, installed, attested or ready (default)",
      "enum": [
        "none",
        "installed",
        "attested",
        "ready"
      ],
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `delete_network`

```text
Delete a virtual network. Fails with conflict while any VM, running or stopped, is attached to it.

delete_vm those first; list_networks shows the leases.
```

```json
{
  "properties": {
    "name": {
      "description": "Network name as returned by create_network or list_networks",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `delete_vm`

```text
Delete a VM, stopping it if it runs: its disk, vTPM state, lease, port forwards and record. Not undoable.

Call it for failed VMs too; afterwards delete_network can succeed.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `exec_vm`

```text
Run a command as root inside a VM over ssh through the virtual network. Anything the command does happens for real.

Authenticated with vm-manager's per-VM key; the guest's host key is pinned on first use. Needs a VM that is ready or running with sshd up. A non-zero exit is a result (exitCode), not an error.
```

```json
{
  "properties": {
    "command": {
      "description": "Program and arguments, e.g. [\"systemctl\", \"is-system-running\"]",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id",
    "command"
  ],
  "type": "object"
}
```

## `forward_port`

```text
Expose one guest TCP port (e.g. 6443 for the API server, 22 for ssh) on a loopback address of the host.

Returns that address. Each call opens a new forward, which lives until the VM is deleted; the VM needs an IP, which it has once created.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    },
    "port": {
      "description": "Guest TCP port, 1-65535",
      "type": "number"
    }
  },
  "required": [
    "id",
    "port"
  ],
  "type": "object"
}
```

## `get_host`

```text
Report whether this KVM host can run VMs (ready, missing prerequisites) and its capabilities. Call it before create_vm.

Also the hostname, kernel, CPUs and memory; access to /dev/kvm and /dev/vhost-vsock; the qemu, swtpm and systemd versions; the storage providers; and the OVMF firmware VMs boot with, whose build golden PCR values must have been recorded with.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `get_image`

```text
Describe one VM image: its artifacts, the Kubernetes versions it offers as a sysext and its PCR policy.

list_images has the references.
```

```json
{
  "properties": {
    "ref": {
      "description": "Image reference: \"<id>_<version>\" or the bare id for its newest version",
      "type": "string"
    }
  },
  "required": [
    "ref"
  ],
  "type": "object"
}
```

## `get_info`

```text
Report this server's build (version, commit, build time) and the names of its tools.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `get_network`

```text
Describe one virtual network: spec, gateway and leases.

list_networks has the names.
```

```json
{
  "properties": {
    "name": {
      "description": "Network name as returned by create_network or list_networks",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `get_vm`

```text
Fetch one VM: state, IP and MAC, attestation summary, the guest's last STATUS= text, and lastError on failure.

States: creating, installing, booting, attesting, ready, running, stopping, stopped, failed, deleting. Poll it after create_vm with wait_for none, or after start_vm and reboot_vm.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `get_vm_attestation`

```text
Return the attestation of a VM's current boot: whether required, whether user-data was released, and the quote verdicts.

The initrd quote gates user-data; the ready quote includes PCR 13. Reset on every installed boot.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `get_vm_console`

```text
Return the last lines of a VM's serial console, the first place to look when a VM failed or hangs installing or booting.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    },
    "lines": {
      "description": "Number of trailing lines, default 100, at most 10000",
      "type": "number"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `get_vm_metrics`

```text
Report one VM's metrics: its QEMU process, disk, install and boot times, network traffic, and the guest's last report.

host has the process's cpu_seconds and memory_rss_bytes (absent while no process runs), disk_bytes, install_seconds, boot_to_ready_seconds and the network's byte counters. guest summarizes the guest's last systemd-report upload, null until one arrived. Every series is also on the Prometheus /metrics endpoint; the full upload is REST-only at raw_report_url.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `list_images`

```text
List the bootable VM images in the catalog and the Kubernetes versions each offers. create_vm needs at least one.

Empty means no image was built or --image-dir points elsewhere.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `list_networks`

```text
List the virtual networks with their gateway and the leases (VM id, MAC, IP) of attached VMs.

The server creates a network named default at startup.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `list_vms`

```text
List every VM, oldest first: id, name, state, IP, image, sizing, attestation and timestamps.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `reboot_vm`

```text
Reboot a VM: stop_vm followed by start_vm.

Returns the record in state booting; poll get_vm.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `start_vm`

```text
Boot a stopped or failed VM from its installed disk; attestation runs again and gates user-data again.

Returns the record in state booting; poll get_vm for ready. Fails with conflict when the VM is running or never finished installing.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```

## `stop_vm`

```text
Shut a running VM down: ACPI power-down, then SIGTERM and SIGKILL after the stop timeout. Its disk is kept.

Returns the record in state stopping; get_vm shows stopped once the process exited. start_vm boots it again.
```

```json
{
  "properties": {
    "id": {
      "description": "VM id as returned by create_vm or list_vms",
      "type": "string"
    }
  },
  "required": [
    "id"
  ],
  "type": "object"
}
```
