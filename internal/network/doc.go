// Package network is vm-manager's rootless software-defined network. Every
// network is one gvisor-tap-vsock VirtualNetwork: a userspace Ethernet switch
// plus a gVisor TCP/IP stack that acts as the network's gateway, DHCP server,
// DNS forwarder and NAT. No tap devices, bridges, iptables or CAP_NET_ADMIN
// are involved, so the same code runs on a developer laptop, in a rootless
// container and on a GitHub Actions runner.
//
// # Network model
//
// A [Manager] owns the networks of one vm-manager process. Each [Network] is
// a separate L2 segment; VMs on different networks cannot see each other even
// when their CIDRs overlap, because every network has its own switch and
// stack. QEMU attaches to a network through a unix socket under the state
// dir, <state>/networks/<name>/qemu.sock, speaking the QEMU "stream" netdev
// framing (each Ethernet frame prefixed by its 32-bit big-endian length). A
// goroutine accepts connections for the lifetime of the network, so any
// number of VMs can join and a VM whose vm-manager restarted simply
// reconnects (QEMU is started with reconnect-ms).
//
// # Addressing
//
// For a network with CIDR a.b.c.0/24:
//
//	a.b.c.0     network address
//	a.b.c.1     gateway: default route, DHCP server, DNS forwarder (Spec.GatewayIP overrides)
//	a.b.c.2-253 pool handed to VMs by Attach, lowest free address first
//	a.b.c.254   host alias: the VM reaches services bound to the host's loopback here
//	a.b.c.255   broadcast
//
// The IMDS address 169.254.169.254 is a virtual IP of the gateway: the
// gateway answers ARP for it, and [Network.ListenIMDS] opens a TCP listener
// on 169.254.169.254:80 inside the stack, so the guest talks to the metadata
// service exactly as it would on a cloud. Connections to the IMDS address
// without a listener are refused rather than forwarded to the host.
//
// MAC addresses are derived, not random: 02:xx:aa:bb:cc:dd where 02 marks a
// locally administered unicast address, xx hashes the network name and the
// remaining four bytes are the IPv4 address (the convention Docker uses).
// DHCP leases are therefore static by construction: the network pre-registers
// every pool address with its MAC in the gateway's DHCP server, [Network.Attach]
// picks a free slot and returns its MAC and IP, and the guest's DHCP DISCOVER
// gets exactly that IP back. This is also why vm-manager never lets the
// library allocate dynamically: gvisor-tap-vsock only accepts static leases
// when the stack is created, and a network must keep serving running VMs
// while new ones attach. A MAC the network does not know gets no lease.
//
// Which VM holds which address is the only mutable state; [Network.State]
// exports it and [Manager.Restore] rebuilds networks from it after a restart,
// so a VM keeps its IP (and MAC) for its whole life. Persisting the state is
// the caller's job.
//
// # What the runtime passes to QEMU
//
// [Attachment.QEMUArgs] yields the two arguments the QEMU runtime appends:
//
//	-netdev stream,id=net0,addr.type=unix,addr.path=<state>/networks/<name>/qemu.sock,reconnect-ms=1000
//	-device virtio-net-pci,netdev=net0,mac=02:xx:aa:bb:cc:dd
//
// The guest sees a plain virtio NIC and configures it with DHCP; the reply
// carries the gateway as router and DNS server, the subnet mask, the MTU and
// Spec.DNSSearchDomain. DNS names are resolved by the host's resolver.
//
// # Host access
//
// Outbound traffic from a VM is dialed from the host process, so the VM can
// reach whatever the host can reach (NAT). The host alias address is
// translated to 127.0.0.1 so a VM can also use services bound to the host's
// loopback. In the other direction [Network.Dial] opens a TCP connection into
// the network from the host (ssh, kube-apiserver readiness) and
// [Network.Forward] exposes a VM port on a host address with a plain
// accept-and-proxy loop.
//
// # Limitations
//
// Every packet is copied through userspace and terminated by a Go TCP stack;
// expect hundreds of Mbit/s rather than line rate, and no UDP or ICMP from
// the host into the network. gvisor-tap-vsock cannot stop a stack once
// created, so the goroutines of a deleted network's DHCP and DNS servers
// linger until the process exits, and the library logs through the global
// logrus logger, which the command wiring should redirect. Only IPv4 is
// supported.
//
// A kernel-backed backend (tap devices on a bridge with systemd-networkd
// doing DHCP and masquerading) would implement the same Manager and Network
// API: Attach would return "-netdev tap,fd=N" style arguments instead of the
// stream socket, Dial and Forward would use the host's own stack, and
// ListenIMDS would bind 169.254.169.254 on the bridge. Nothing in
// internal/vm needs to know which backend is active.
package network
