# agentbox topology

The original box's surroundings, with private details replaced by examples. When recreating a box, get the owner's answers first (see "Recreating this box" in SKILL.md) and rewrite this file with them. Treat addresses as facts to verify (`ip`, `tailscale status`, `agentbox-facts`) before relying on them.

## Hypervisor host

| | |
|---|---|
| Node | Proxmox VE node `pve`, LAN address `192.168.1.10` (web UI :8006, root SSH: owner only) |
| Hardware | small 4-core mini PC, 32 GB RAM, NVMe |
| Storage | `local` (ISOs, templates, backups) and `local-lvm` (VM disks) |
| Backups | vzdump of every VM nightly, snapshot mode, keep the last copy plus a weekly one |

VMs on the host:

| ID | Name | RAM | Notes |
|---|---|---|---|
| 101 | (production VM, e.g. home automation) | 4 GiB | Anything that loads the host hurts it. Agents never touch it. |
| 102 | agentbox | 20 GiB, ballooning disabled | This VM. 128 GB disk plus EFI disk. Starts on host boot. |
| 103 | (other VM) | 4 GiB | |

Leave a few GiB of physical memory unallocated for the hypervisor and QEMU overhead. A fixed RAM size (no ballooning) keeps `free` honest for agents sizing their work.

The VM has a serial console (`qm terminal 102`) and a web console, both with root autologin: that is the owner's recovery path if SSH or networking breaks.

## Home network

| | |
|---|---|
| LAN | `192.168.1.0/24`, DHCP and DNS from the router |
| agentbox | `192.168.1.50` (DHCP reservation), mDNS `agentbox.local` via systemd-resolved |
| WAN | Often carrier-grade NAT: no inbound port forwarding, so remote access goes through Tailscale or a Cloudflare Tunnel. |

## Ways in

| Path | Address | Notes |
|---|---|---|
| LAN SSH / mosh | `owner@agentbox.local` | Fastest; `ssh agentbox` alias on the owner's laptop. |
| Tailscale | `owner@100.x.y.z` | From anywhere on the tailnet. With MagicDNS off, use the 100.x address. |
| Cloudflare Tunnel | `agentbox.example.com` to `ssh://localhost:22` | For devices that cannot run Tailscale (for example a managed work laptop). Protected by a Cloudflare Access application plus SSH keys. Optional HTTP hostnames (hub, desktops, telemetry) sit behind their own Access application. |
| Hypervisor console | host side only | Root autologin on tty and serial. |

SSH accepts keys only (no passwords, no root login). Keep one authorized key per owner device and label each key with its device.

## The owner's devices

| Device | Notes |
|---|---|
| Main laptop | Terminal (e.g. Ghostty) into Zellij on the box. If it is company-managed and blocks Tailscale, use the tunnel; agents never suggest ways around an employer's filtering. |
| Phone | Tailscale peer plus an SSH client. |

## Outbound identity

- A GitHub account with an SSH key and a GPG signing key generated on the box; commits use the account's noreply address.
- A domain on the owner's Cloudflare account, if the tunnel is used. Other tunnels on the same account are off limits to agents.
