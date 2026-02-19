# openclaw-vps

Pulumi infrastructure for deploying [OpenClaw](https://github.com/coollabsio/openclaw) on an AWS spot instance with Tailscale networking.

## What it does

Provisions a single EC2 spot instance (with on-demand fallback) that:

- Joins your Tailscale network and exposes OpenClaw via `tailscale serve`
- Attaches a persistent EBS volume for data that survives spot interruptions
- Runs OpenClaw and a headless Chrome browser via Docker Compose
- Takes daily EBS snapshots (retains 7 days)
- Has no public-facing ports — only Tailscale WireGuard UDP is open

## Prerequisites

- [Pulumi CLI](https://www.pulumi.com/docs/install/)
- [Go 1.25+](https://go.dev/dl/)
- AWS credentials configured (`aws configure` or environment variables)
- A [Tailscale auth key](https://login.tailscale.com/admin/settings/keys)
- An [Anthropic API key](https://console.anthropic.com/)

## Setup

```bash
# Initialize a stack
pulumi stack init dev

# Configure required secrets
pulumi config set --secret openclaw-vps:tailscaleAuthKey <your-tailscale-key>
pulumi config set --secret openclaw-vps:anthropicApiKey <your-anthropic-key>

# Optional: change instance type (default: t3.medium)
pulumi config set openclaw-vps:instanceType t3.large
```

## Deploy

```bash
pulumi up
```

## Access

Once deployed, the instance joins your Tailscale network as `openclaw`. Access OpenClaw at:

```
https://openclaw.<your-tailnet-name>.ts.net
```

You can also SSH in via Tailscale:

```bash
ssh openclaw
```

## Infrastructure

| Resource | Purpose |
|---|---|
| Auto Scaling Group | Maintains exactly 1 spot instance (on-demand fallback) |
| EBS Volume | Persistent 10 GB gp3 volume for OpenClaw data |
| Security Group | Allows only Tailscale UDP (port 41641) inbound |
| IAM Role | Permits EBS attach/detach and snapshot management |
| Launch Template | Ubuntu 24.04, user-data bootstrap, 20 GB root volume |

## Stack outputs

| Output | Description |
|---|---|
| `asgName` | Auto Scaling Group name |
| `volumeId` | Persistent EBS volume ID |
| `securityGroupId` | Security group ID |
| `availabilityZone` | AZ where the instance and volume are placed |
