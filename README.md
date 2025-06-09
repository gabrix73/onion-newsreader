# 🧅 Secure NNTP Newsreader

[![Go Version](https://img.shields.io/badge/Go-1.19+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Security](https://img.shields.io/badge/Security-Hardened-red.svg)](#security-features)

A **privacy-focused**, **security-hardened** NNTP newsreader designed for anonymous Usenet access with integrated posting capabilities via [m2usenet](https://github.com/gabrix73/m2usenet-go).

## 🔒 Security Features

- **HTML Escaping**: All user content is properly escaped to prevent XSS attacks
- **Input Validation**: Strict validation of group names, article numbers, and headers
- **Connection Isolation**: Each request uses isolated NNTP connections
- **No Data Persistence**: No logs, cookies, or user data stored on server
- **Rate Limiting Ready**: Designed for integration with reverse proxy rate limiting
- **Tor-Friendly**: Optimized for .onion deployment and Tor hidden services
- **CSRF Protection**: Stateless design prevents cross-site request forgery
- **Memory Safety**: Go's memory safety prevents buffer overflows and injection attacks

## 📋 Features

### Core Functionality
- 📰 **Group Browsing**: Browse available newsgroups with article counts
- 📖 **Article Reading**: Read individual articles with proper threading
- 🔍 **Header Analysis**: Full RFC-compliant header parsing and display
- 💬 **Reply Integration**: Seamless integration with m2usenet posting service
- 🎨 **Responsive Design**: Mobile-friendly interface with dark/light themes

### Advanced Features
- **Quote Detection**: Automatic styling of quoted text in replies
- **Signature Handling**: Proper display of message signatures
- **URL Highlighting**: Safe URL detection without clickable links
- **Thread Navigation**: Navigate between articles in the same thread
- **Multi-Group Support**: Handle crossposted articles correctly

## 🚀 Installation

### Prerequisites
```bash
# Debian/Ubuntu
sudo apt update
sudo apt install golang-go git

# CentOS/RHEL/Fedora
sudo dnf install golang git

# Arch Linux
sudo pacman -S go git
```

### 1. Clone Repository
```bash
git clone https://github.com/yourusername/newsreader.git
cd newsreader
```

### 2. Security-Hardened Compilation

#### Standard Compilation
```bash
# Basic build
go build -o newsreader main.go
```

#### Production Security Build
```bash
# Security-hardened build with all protections
go build -ldflags="-s -w -extldflags=-static" \
         -a -installsuffix cgo \
         -tags netgo,osusergo \
         -trimpath \
         -buildmode=exe \
         -o newsreader main.go

# Strip additional symbols (optional)
strip newsreader
```

#### Build Flags Explained
- `-ldflags="-s -w"`: Strip debugging info and symbol table
- `-extldflags=-static`: Static linking for isolated deployment
- `-a -installsuffix cgo`: Force rebuild of all packages
- `-tags netgo,osusergo`: Pure Go networking (no C dependencies)
- `-trimpath`: Remove absolute paths from binary
- `-buildmode=exe`: Explicit executable mode
- `strip`: Remove additional symbols for smaller binary

### 3. Configuration

Create configuration file:
```bash
cp config.example.json config.json
```

Edit `config.json`:
```json
{
  "nntp_server": "peannyjkqwqfynd24p6dszvtchkq7hfkwymi5by5y332wmosy5dwfaqd.onion:119",
  "nntp_username": "your_username",
  "nntp_password": "your_password",
  "listen_port": 8080,
  "listen_address": "127.0.0.1",
  "m2usenet_url": "http://itcxzfm2h36hfj6j7qxksyfm4ipp3co4rkl62sgge7hp6u77lbretiyd.onion:8880",
  "max_articles_per_group": 500,
  "connection_timeout": 30,
  "read_timeout": 60
}
```

## 🔧 SystemD Service

### 1. Create Service User
```bash
# Create dedicated system user (security best practice)
sudo useradd --system --shell /usr/sbin/nologin --home-dir /var/lib/newsreader newsreader
sudo mkdir -p /var/lib/newsreader
sudo chown newsreader:newsreader /var/lib/newsreader
```

### 2. Install Binary and Config
```bash
# Install binary
sudo cp newsreader /usr/local/bin/
sudo chmod 755 /usr/local/bin/newsreader
sudo chown root:root /usr/local/bin/newsreader

# Install configuration
sudo mkdir -p /etc/newsreader
sudo cp config.json /etc/newsreader/
sudo chmod 640 /etc/newsreader/config.json
sudo chown root:newsreader /etc/newsreader/config.json
```

### 3. SystemD Service File

Create `/etc/systemd/system/newsreader.service`:

```ini
[Unit]
Description=Secure NNTP Newsreader Service
Documentation=https://github.com/gabrix73/onion-newsreader
After=network-online.target
Wants=network-online.target
ConditionFileNotEmpty=/etc/newsreader/config.json

[Service]
Type=simple
User=newsreader
Group=newsreader
ExecStart=/usr/local/bin/newsreader -config=/etc/newsreader/config.json
ExecReload=/bin/kill -HUP $MAINPID
KillMode=mixed
KillSignal=SIGTERM
TimeoutStopSec=30
Restart=always
RestartSec=10

# Security Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
RemoveIPC=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectHostname=yes
ProtectClock=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_INET AF_INET6
SystemCallFilter=@system-service
SystemCallFilter=~@debug @mount @cpu-emulation @obsolete @reboot @swap @privileged @resources
SystemCallErrorNumber=EPERM

# Resource Limits
LimitNOFILE=1024
LimitNPROC=512
MemoryMax=512M
CPUQuota=50%

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=newsreader

# Working Directory
WorkingDirectory=/var/lib/newsreader
ReadWritePaths=/var/lib/newsreader

[Install]
WantedBy=multi-user.target
```

### 4. Enable and Start Service

```bash
# Reload systemd configuration
sudo systemctl daemon-reload

# Enable service to start on boot
sudo systemctl enable newsreader

# Start service
sudo systemctl start newsreader

# Check status
sudo systemctl status newsreader

# View logs
sudo journalctl -u newsreader -f
```

## 🛡️ SystemD Security Features Explained

### Process Isolation
- **`NoNewPrivileges=yes`**: Prevents privilege escalation
- **`PrivateTmp=yes`**: Isolated /tmp directory
- **`PrivateDevices=yes`**: No access to /dev devices
- **`ProtectHome=yes`**: No access to user home directories

### System Protection
- **`ProtectSystem=strict`**: Read-only access to /usr, /boot, /efi
- **`ProtectKernelTunables=yes`**: No access to kernel parameters
- **`ProtectKernelModules=yes`**: Cannot load kernel modules
- **`ProtectControlGroups=yes`**: No access to cgroup filesystem

### Network & System Calls
- **`RestrictAddressFamilies=AF_INET AF_INET6`**: Only IPv4/IPv6 networking
- **`SystemCallFilter=@system-service`**: Whitelist essential system calls
- **`SystemCallFilter=~@debug @mount...`**: Blacklist dangerous system calls
- **`MemoryDenyWriteExecute=yes`**: Prevents code injection attacks

### Resource Limits
- **`MemoryMax=512M`**: Maximum memory usage
- **`CPUQuota=50%`**: Maximum CPU usage
- **`LimitNOFILE=1024`**: Maximum open files
- **`LimitNPROC=512`**: Maximum processes

## 🔐 Security Best Practices

### Server Deployment
1. **Reverse Proxy**: Deploy behind nginx/apache with HTTPS
2. **Rate Limiting**: Implement connection and request rate limits
3. **Tor Integration**: Consider .onion deployment for privacy
4. **Firewall**: Restrict access to necessary ports only
5. **Updates**: Keep Go runtime and dependencies updated

### Configuration Security
```bash
# Secure configuration file permissions
sudo chmod 640 /etc/newsreader/config.json
sudo chown root:newsreader /etc/newsreader/config.json

# Verify service isolation
sudo systemd-analyze security newsreader
```

### Monitoring Commands
```bash
# Monitor service status
sudo systemctl status newsreader

# View real-time logs
sudo journalctl -u newsreader -f

# Check resource usage
sudo systemctl show newsreader --property=MemoryCurrent,CPUUsageNSec

# Verify security settings
sudo systemd-analyze security newsreader
```

## 🌐 Integration with m2usenet

This newsreader seamlessly integrates with [m2usenet](https://github.com/gabrix73/m2usenet-go) for posting:

1. **Read Articles**: Browse and read Usenet content
2. **Reply Function**: Click "REPLY TO POST" button  
3. **Auto-redirect**: Opens m2usenet with reply context
4. **Secure Posting**: Uses hashcash PoW and Ed25519 signatures

### Reply Integration Features
- ✅ Automatic subject handling (avoids "Re: Re:" duplicates)
- ✅ Proper Message-ID references for threading
- ✅ Quoted content formatting
- ✅ Cross-group posting support

## 📚 Usage Examples

### Basic Usage
```bash
# Start the service
sudo systemctl start newsreader

# Access via web browser
curl http://localhost:8080

# View specific group
curl http://localhost:8080/group/alt.privacy
```

### Security Testing
```bash
# Test service isolation
sudo systemd-run --uid=newsreader --gid=newsreader \
  --property=ProtectSystem=strict \
  --property=PrivateTmp=yes \
  /usr/local/bin/newsreader -config=/etc/newsreader/config.json

# Verify no privilege escalation
sudo -u newsreader /usr/local/bin/newsreader -config=/etc/newsreader/config.json
```

## 🐛 Troubleshooting

### Common Issues

**Service won't start:**
```bash
# Check configuration
sudo newsreader -config=/etc/newsreader/config.json -test

# Verify permissions
ls -la /etc/newsreader/config.json
ls -la /usr/local/bin/newsreader

# Check service logs
sudo journalctl -u newsreader -n 50
```

**NNTP connection issues:**
```bash
# Test network connectivity
telnet news.server.com 119

# Check firewall
sudo ufw status
sudo iptables -L
```

**Permission issues:**
```bash
# Reset permissions
sudo chown root:newsreader /etc/newsreader/config.json
sudo chmod 640 /etc/newsreader/config.json
sudo chown newsreader:newsreader /var/lib/newsreader
```

## 🤝 Contributing

1. Fork the repository
2. Create feature branch (`git checkout -b feature/amazing-feature`)
3. Commit changes (`git commit -m 'Add amazing feature'`)
4. Push to branch (`git push origin feature/amazing-feature`)
5. Open Pull Request

### Security Issues
Report security vulnerabilities privately to: [security@yourdomain.com](mailto:security@yourdomain.com)

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## 🔗 Related Projects

- [m2usenet](https://github.com/gabrix73/m2usenet-go) - Anonymous Usenet posting gateway
- [Tor Project](https://www.torproject.org/) - Anonymous networking

## ⚠️ Disclaimer

This software is intended for legitimate Usenet access and educational purposes. Users are responsible for complying with their local laws and the terms of service of their NNTP providers. The authors assume no responsibility for misuse of this software.

---

**🔒 Built with Security in Mind | 🧅 Tor-Ready | 📰 RFC-Compliant**
