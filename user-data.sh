#!/bin/bash
set -x

# --- Injected by Pulumi ---
EBS_VOLUME_ID="{{.EBSVolumeID}}"
TAILSCALE_AUTH_KEY="{{.TailscaleAuthKey}}"
ANTHROPIC_API_KEY="{{.AnthropicApiKey}}"
DOCKER_COMPOSE_B64="{{.DockerComposeB64}}"

MOUNT_POINT="/mnt/openclaw-data"
DEVICE="/dev/xvdf"
COMPOSE_DIR="/opt/openclaw"

echo "=== OpenClaw VPS Bootstrap ==="

# 1. Install and configure Tailscale FIRST (so we can SSH in to debug)
echo "Installing Tailscale..."
curl -fsSL https://tailscale.com/install.sh | sh
tailscale up --authkey="$TAILSCALE_AUTH_KEY" --ssh --hostname=openclaw --force-reauth

# 2. Install Docker
echo "Installing Docker..."
apt-get update -y
apt-get install -y ca-certificates curl gnupg
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" > /etc/apt/sources.list.d/docker.list
apt-get update -y
apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin

systemctl enable docker
systemctl start docker

# 3. Attach and mount EBS volume
echo "Attaching EBS volume..."
IMDS_TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# Install AWS CLI v2
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /tmp/awscliv2.zip
apt-get install -y unzip
unzip -q /tmp/awscliv2.zip -d /tmp
/tmp/aws/install
rm -rf /tmp/awscliv2.zip /tmp/aws
aws ec2 attach-volume --volume-id "$EBS_VOLUME_ID" --instance-id "$INSTANCE_ID" --device "$DEVICE" --region "$REGION"

echo "Waiting for device..."
for i in $(seq 1 30); do
    if [ -b "$DEVICE" ] || [ -b /dev/nvme1n1 ]; then
        break
    fi
    sleep 2
done

# Handle NVMe device naming on Nitro instances
ACTUAL_DEVICE="$DEVICE"
if [ -b /dev/nvme1n1 ] && [ ! -b "$DEVICE" ]; then
    ACTUAL_DEVICE="/dev/nvme1n1"
fi

# Format if new (no filesystem)
if ! blkid "$ACTUAL_DEVICE" > /dev/null 2>&1; then
    echo "Formatting new EBS volume..."
    mkfs.ext4 "$ACTUAL_DEVICE"
fi

mkdir -p "$MOUNT_POINT"
mount "$ACTUAL_DEVICE" "$MOUNT_POINT"

# Add to fstab for remounts
echo "$ACTUAL_DEVICE $MOUNT_POINT ext4 defaults,nofail 0 2" >> /etc/fstab

# 4. Write docker-compose.yml
echo "Writing docker-compose.yml..."
mkdir -p "$COMPOSE_DIR"
echo "$DOCKER_COMPOSE_B64" | base64 -d > "$COMPOSE_DIR/docker-compose.yml"

# 5. Generate gateway token (first boot only, persists on EBS)
if [ ! -f "$MOUNT_POINT/.gateway-token" ]; then
    openssl rand -hex 32 > "$MOUNT_POINT/.gateway-token"
    chmod 600 "$MOUNT_POINT/.gateway-token"
fi

# 6. Write .env for docker compose
cat > "$COMPOSE_DIR/.env" <<ENVEOF
OPENCLAW_GATEWAY_TOKEN=$(cat "$MOUNT_POINT/.gateway-token")
ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
ENVEOF
chmod 600 "$COMPOSE_DIR/.env"

# 7. Start OpenClaw
echo "Starting OpenClaw..."
cd "$COMPOSE_DIR"
docker compose up -d

# 6. Configure Tailscale Serve
echo "Configuring Tailscale Serve..."
tailscale serve --bg 8080

# 7. Daily EBS snapshot backup (keeps last 7)
echo "Setting up daily EBS snapshot cron..."
cat > /etc/cron.daily/openclaw-backup <<'CRONEOF'
#!/bin/bash
IMDS_TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" http://169.254.169.254/latest/meta-data/placement/region)
VOLUME_ID="VOLUME_ID_PLACEHOLDER"
SNAP_ID=$(aws ec2 create-snapshot --volume-id "$VOLUME_ID" --description "openclaw daily backup $(date +%Y-%m-%d)" --region "$REGION" --query 'SnapshotId' --output text)
aws ec2 create-tags --resources "$SNAP_ID" --tags Key=Name,Value=openclaw-daily-backup --region "$REGION"

# Delete snapshots older than 7 days
aws ec2 describe-snapshots --owner-ids self --filters "Name=tag:Name,Values=openclaw-daily-backup" --region "$REGION" --query 'Snapshots[?StartTime<=`'"$(date -d '7 days ago' --iso-8601)"'`].SnapshotId' --output text | tr '\t' '\n' | while read -r old; do
    [ -n "$old" ] && aws ec2 delete-snapshot --snapshot-id "$old" --region "$REGION"
done
CRONEOF
# Inject actual volume ID
sed -i "s/VOLUME_ID_PLACEHOLDER/$EBS_VOLUME_ID/" /etc/cron.daily/openclaw-backup
chmod +x /etc/cron.daily/openclaw-backup

echo "=== OpenClaw VPS Bootstrap Complete ==="
