package gcp

import "fmt"

// proxyConfigurationScript is the initialization script for a standard HTTP proxy VM
// Adapted from AWS version for GCP VMs running CentOS/RHEL
const proxyConfigurationScript = `#!/bin/bash
# Install and configure squid proxy on GCP VM
yum update -y
yum install -y squid

# Configure squid to allow HTTP CONNECT on all ports (not just 443)
# By default, squid only allows connect on port 443
sed -E 's/(^http_access deny CONNECT.*)/#\1/' -i /etc/squid/squid.conf

# Enable and start squid service
systemctl enable squid
systemctl start squid

# Configure SSH access for the default user
mkdir -p /home/centos/.ssh
chmod 0700 /home/centos/.ssh
echo -e '%s' >/home/centos/.ssh/authorized_keys
chmod 0600 /home/centos/.ssh/authorized_keys
chown -R centos:centos /home/centos/.ssh

# Configure firewall to allow proxy traffic
firewall-cmd --permanent --add-port=3128/tcp
firewall-cmd --reload
`

// secureProxyConfigurationScript is the initialization script for a secure HTTPS proxy VM
// Uses mitmproxy for HTTPS inspection and certificate generation
const secureProxyConfigurationScript = `#!/bin/bash
# Install and configure mitmproxy for secure proxy on GCP VM
yum update -y
yum install -y curl tar gzip

# Download and install mitmproxy
curl -OL https://snapshots.mitmproxy.org/7.0.2/mitmproxy-7.0.2-linux.tar.gz
tar xzvf mitmproxy-7.0.2-linux.tar.gz -C /usr/bin
rm mitmproxy-7.0.2-linux.tar.gz

# Create mitmproxy startup script
cat <<EOF > /usr/bin/run-mitm
#!/bin/bash
mitmdump --showhost --ssl-insecure \
  -p 3128 \
  --ignore-hosts '.*'
EOF

chmod +x /usr/bin/run-mitm

# Create systemd service for mitmproxy
cat <<EOF > /lib/systemd/system/mitmproxy.service
[Unit]
Description=mitmdump service
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/bin/run-mitm
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF

# Enable and start mitmproxy service
systemctl daemon-reload
systemctl enable mitmproxy
systemctl start mitmproxy

# Configure SSH access for the default user
mkdir -p /home/centos/.ssh
chmod 0700 /home/centos/.ssh
echo -e '%s' >/home/centos/.ssh/authorized_keys
chmod 0600 /home/centos/.ssh/authorized_keys
chown -R centos:centos /home/centos/.ssh

# Configure firewall to allow proxy traffic
firewall-cmd --permanent --add-port=3128/tcp
firewall-cmd --reload
`

// proxyConfigScript returns the appropriate proxy configuration script
// based on whether secure proxy is enabled
func proxyConfigScript(isSecure bool, publicSSHKey string) string {
	if isSecure {
		return fmt.Sprintf(secureProxyConfigurationScript, publicSSHKey)
	}
	return fmt.Sprintf(proxyConfigurationScript, publicSSHKey)
}