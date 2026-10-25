# wberg networks speedtest

A small Go server that serves download test files (100MB, 1GB, 5GB, 10GB, 100GB) over HTTP on port 80 and HTTPS on port 443. HTTPS uses a Let's Encrypt certificate that the server obtains and renews itself.

The files are never stored on disk. Their content is a 4 MiB random block, generated at startup and repeated, so it can't be compressed. Range requests and resume are supported. Each IPv4 address or IPv6 /64 can run at most 4 downloads at once (change this with `-per-ip`).

## Build

```sh
go build -o speedtest .
```

## Run locally (HTTP only)

```sh
./speedtest -http :8080
curl -o /dev/null http://localhost:8080/1GB.bin
```

## Deploy

1. Point DNS (A/AAAA) for your hostname at the server. Ports 80 and 443 must be reachable from the internet so the certificate can be issued.
2. Install the binary and the systemd unit:
   ```sh
   sudo install -m 755 speedtest /usr/local/bin/
   sudo cp speedtest.service /etc/systemd/system/
   ```
3. Edit `ExecStart` in `/etc/systemd/system/speedtest.service` and set your hostname: `-domain speedtest.example.net`. You can optionally add `-email you@example.net`.
4. Start the service:
   ```sh
   sudo systemctl daemon-reload
   sudo systemctl enable --now speedtest
   journalctl -u speedtest -f
   ```

The unit runs as an unprivileged dynamic user and gets `CAP_NET_BIND_SERVICE` so it can bind ports 80/443. Certificates are cached in `/var/lib/speedtest/autocert`.

## Certificates

Certificates are renewed automatically by the running service, about 30 days before they expire. No certbot or cron job is needed. Renewal requires that the service is running, port 443 (or 80) is reachable from the internet, and DNS still points at the server.

Check the expiry date:

```sh
echo | openssl s_client -connect speedtest.example.net:443 -servername speedtest.example.net 2>/dev/null | openssl x509 -noout -dates
```

Force a new certificate (normally never needed):

```sh
sudo systemctl stop speedtest
sudo rm /var/lib/speedtest/autocert/speedtest.example.net*
sudo systemctl start speedtest
curl -sI https://speedtest.example.net/ >/dev/null
```

Avoid doing this repeatedly: Let's Encrypt allows 5 duplicate certificates per week.

## Network tuning

`sysctl/90-speedtest.conf` tunes the kernel for high-throughput downloads:

- BBR congestion control with `fq` pacing
- TCP buffers up to 64 MB, so a single stream can fill a fast long-distance link
- No slow start after idle, and MTU probing
- Larger connection backlogs

Install it and apply it without rebooting:

```sh
sudo cp sysctl/90-speedtest.conf /etc/sysctl.d/
sudo sysctl --system
```

Check that it is active:

```sh
cat /proc/sys/net/ipv4/tcp_congestion_control    # bbr
cat /proc/sys/net/core/default_qdisc             # fq
```

## Logging

The server writes one line per download (client IP, file, status, bytes, duration, Mbit/s). By default these go to the journal:

```sh
journalctl -u speedtest -f
```

To write them to `/var/log/speedtest.log` instead, add this to the `[Service]` section of the unit:

```
StandardOutput=append:/var/log/speedtest.log
StandardError=append:/var/log/speedtest.log
```

After any change to the unit, apply it:

```sh
sudo systemctl daemon-reload
sudo systemctl restart speedtest
```

Rotate the file with `/etc/logrotate.d/speedtest`:

```
/var/log/speedtest.log {
    weekly
    rotate 8
    compress
    missingok
    notifempty
    copytruncate
}
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-domain` | (empty) | Comma-separated hostnames. If empty, the server runs HTTP only. |
| `-email` | (empty) | ACME contact email. |
| `-cache` | `/var/lib/speedtest/autocert` | Certificate cache directory. |
| `-http` | `:80` | HTTP listen address. |
| `-https` | `:443` | HTTPS listen address. |
| `-per-ip` | `4` | Max concurrent downloads per IPv4 address or IPv6 /64. |

## Usage

```sh
curl -o /dev/null https://speedtest.example.net/10GB.bin
wget -O /dev/null http://speedtest.example.net/1GB.bin
```

HTTPS uses HTTP/1.1 only. Go's HTTP/2 flow control can cap the speed of a single stream.
