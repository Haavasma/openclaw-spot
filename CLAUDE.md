# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Pulumi (Go) infrastructure-as-code project that deploys an OpenClaw instance on an AWS spot instance with Tailscale networking. The entire infrastructure is defined in a single `main.go` file.

## Architecture

- **main.go**: Pulumi program that provisions an AWS Auto Scaling Group (single spot instance with on-demand fallback), a persistent EBS volume, security group (Tailscale UDP only), IAM role/policy, and a launch template.
- **user-data.sh**: Cloud-init bootstrap script templated by Pulumi. Installs Tailscale, Docker, attaches/mounts the EBS volume, deploys docker-compose, configures Tailscale Serve, and sets up daily EBS snapshot backups. Template variables use `{{.VarName}}` syntax.
- **docker-compose.yml**: Runs two containers — `openclaw` (the app, data on `/mnt/openclaw-data`) and `browser` (headless Chrome for CDP). The compose file is base64-encoded and injected into user-data at deploy time.
- **Pulumi.dev.yaml / Pulumi.prod.yaml**: Stack configs. Secrets (Tailscale auth key, Anthropic API key) are encrypted per-stack.

## Commands

```bash
# Preview changes
pulumi preview

# Deploy
pulumi up

# Deploy to a specific stack
pulumi up -s dev
pulumi up -s prod

# View outputs
pulumi stack output

# Set a secret config value
pulumi config set --secret openclaw-vps:tailscaleAuthKey <key>
```

## Key Details

- Instance runs in `eu-north-1` (dev stack), pinned to first available AZ for EBS attachment
- Security group allows only Tailscale WireGuard UDP (port 41641) inbound; all access is via Tailscale
- EBS volume is a separate persistent resource (not deleted with instance); data survives spot interruptions
- Gateway token is auto-generated on first boot and persisted on the EBS volume
