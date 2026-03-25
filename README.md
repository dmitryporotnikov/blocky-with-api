# Blocky DNS Server (Fork)

> **This is a fork of [0xERR0R/blocky](https://github.com/0xERR0R/blocky)** with additional REST API support for dynamic DNS record management.

## Fork Features

This fork extends the original blocky DNS server with:

- **Dynamic DNS Record Management via REST API** - Create, update, delete DNS records at runtime without restarting the server
- **File-based Persistence** - Dynamic records are saved to a YAML file and persist across restarts

---

## Quick Start on Linux

### 1. Build from source

```bash
# Install Go (if not present)
# Ubuntu/Debian: sudo apt install golang-go

# Clone this repository
git clone https://github.com/dmitryporotnikov/blocky-with-api.git
cd blocky-with-api

# Build
go build -o blocky

# Make executable
chmod +x blocky

# Move to your PATH (optional)
sudo mv blocky /usr/local/bin/
```

### 2. Create a configuration File

Create `/etc/blocky/config.yml`:

```yaml
cat /opt/blocky/config.yml
# configuration documentation: https://0xerr0r.github.io/blocky/latest/configuration/

upstreams:
  groups:
    # these external DNS resolvers will be used. Blocky picks 2 random resolvers from the list for each query
    # format for resolver: [net:]host:[port][/path]. net could be empty (default, shortcut for tcp+udp), tcp+udp, tcp, udp, tcp-tls or https (DoH). If port is empty, default port will be used (53 for udp and tcp, 853 for tcp-tls, 443 for https (Doh))
    # this configuration is mandatory, please define at least one external DNS resolver
    default:
      # Adguard1
      - 45.90.28.169
      # Adguard2
      - 45.90.30.169

# optional: use allow/denylists to block queries (for example ads, trackers, adult pages etc.)
blocking:
  # definition of denylist groups. Can be external link (http/https) or local file
  denylists:
    ads:
  #    - https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
  # definition: which groups should be applied for which client
  clientGroupsBlock:
    # default will be used, if no special definition for a client name exists
    default:
      - ads

# optional: write query information (question, answer, client, duration etc.) to daily csv file
queryLog:
  # optional one of: mysql, postgresql, csv, csv-client. If empty, log to console
  type:

# optional: use these DNS servers to resolve denylist urls and upstream DNS servers. It is useful if no system DNS resolver is configured, and/or to encrypt the bootstrap queries.
bootstrapDns:
  - upstream: tcp-tls:one.one.one.one
    ips:
      - 1.1.1.1

# optional: logging configuration
log:
  # optional: Log level (one from trace, debug, info, warn, error). Default: info
  level: info
```

### 3. Create the dynamic records file

Create `/etc/blocky/records.yaml` (can be empty initially):

```yaml
records: {}
```

### 4. Start the Server

```bash
# Run directly
sudo blocky serve -c /etc/blocky/config.yml

# Or run in background with systemd
sudo nohup blocky serve -c /etc/blocky/config.yml > /var/log/blocky.log 2>&1 &
```

### 5. Verify it's working

```bash
# Check DNS resolution
dig @127.0.0.1 google.com

# Check API is responding
curl http://127.0.0.1:4000/api/blocking/status
```

---

## REST API - Dynamic DNS Record Management

The fork adds endpoints to dynamically manage DNS records at runtime.

### Base URL
```
http://localhost:4000/api
```

### Endpoints

#### List All DNS Records
```bash
curl http://localhost:4000/api/dns/records
```

Response:
```json
{
  "records": [
    {"domain": "myserver.local.", "type": "A", "value": "192.168.1.100", "ttl": 3600}
  ]
}
```

#### Add a DNS Record
```bash
curl -X POST http://localhost:4000/api/dns/records \
  -H "Content-Type: application/json" \
  -d '{"type":"A","value":"192.168.1.100","ttl":3600}'
```

Supported record types: `A`, `AAAA`, `TXT`, `CNAME`, `SRV`, `PTR`

#### Update DNS Records for a Domain
```bash
curl -X PUT "http://localhost:4000/api/dns/records/myserver.local." \
  -H "Content-Type: application/json" \
  -d '{"type":"A","value":"192.168.1.200","ttl":7200}'
```

#### Delete All Records for a Domain
```bash
curl -X DELETE "http://localhost:4000/api/dns/records/myserver.local."
```

### Other Existing API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/blocking/status` | Get blocking status |
| GET | `/api/blocking/disable?duration=5m` | Disable blocking for duration |
| GET | `/api/blocking/enable` | Enable blocking |
| POST | `/api/lists/refresh` | Refresh allow/denylists |
| POST | `/api/cache/flush` | Clear DNS cache |
| POST | `/api/query` | Perform DNS query |

---

## Configuration Reference

### Custom DNS with Dynamic File

```yaml
customDNS:
  customTTL: 1h                    # Default TTL for records
  dynamicFile: /path/to/records.yaml  # File for dynamic records
  filterUnmappedTypes: true         # Only return matching record types
  mapping:                         # Static mappings (optional)
    "example.com.":
      - "1.2.3.4"
```

### Dynamic Records File Format

The dynamic records file (`records.yaml`) uses this format:

```yaml
records:
  "myserver.local.":
    - type: A
      value: 192.168.1.100
      ttl: 3600
    - type: TXT
      value: "my text record"
      ttl: 3600
```

---

## Building from Source

```bash
git clone https://github.com/dmitryporotnikov/blocky-with-api.git
cd blocky-with-api
go build -o blocky ./cmd/blocky

# Run
./blocky serve -c config.yml
```

---

## Original Blocky Documentation

For complete blocky documentation including installation, configuration examples, and advanced features, see the [original project documentation](https://0xerr0r.github.io/blocky/).

---

## License

Apache 2.0 - See [original blocky license](https://github.com/0xERR0R/blocky/blob/main/LICENSE)
