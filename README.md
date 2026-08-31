# PProxy

`PProxy` is a small HTTP/HTTPS forward proxy with one routing rule:

- If a connection destination matches `DIRECT_LIST`, send it through the configured upstream proxy.
- Otherwise, connect to the destination directly.

Despite the filename, matching destinations use the upstream proxy. This is intentional and follows the routing behavior described above.

## Requirements

- Go 1.22 or newer (only needed to build)
- An HTTP proxy, by default `http://127.0.0.1:7890`
- A `DIRECT_LIST` file in the working directory

## Run

```sh
go run -buildvcs=false .
```

The local proxy listens at `http://127.0.0.1:7895` by default. Configure an application to use that URL as its HTTP and HTTPS proxy, or try it with curl:

```sh
curl --proxy http://127.0.0.1:7895 http://example.com
curl --proxy http://127.0.0.1:7895 https://example.com
```

Build a standalone binary with:

```sh
go build -buildvcs=false -trimpath -o pproxy .
./pproxy
```

## `DIRECT_LIST` format

Blank lines and lines beginning with `#` are ignored. Each other line may be one of:

```text
# URL: matches its host and port; the path is ignored
http://192.168.12.23:3000/a/path
https://secure.example

# Exact host or IP on any port
example.com
192.168.12.23

# Exact host and port
example.com:8443

# Subdomains only; an optional port can be included
*.internal.example:443

# The domain itself and all subdomains
.service.example

# IP-literal destinations in a CIDR (hostnames are not DNS-resolved for matching)
10.20.0.0/16
```

The file is read once during startup. Restart `pproxy` after changing it.

## Options

```text
-listen address   local listen address (default "127.0.0.1:7895")
-proxy URL        upstream HTTP or HTTPS proxy (default "http://127.0.0.1:7890")
-list path        destination list (default "./DIRECT_LIST")
-verbose          log the selected route for each request
-version          print the version
```

The listener also accepts URL form, for example `-listen http://127.0.0.1:7895`. Proxy credentials can be included in the upstream URL:

```sh
./pproxy -proxy http://username:password@127.0.0.1:7890
```

The default listener is restricted to localhost. Binding to a non-loopback address exposes a forward proxy to other machines and should only be done with appropriate network access controls.

## Start automatically with systemd

Build the binary, link the included user service, and enable it:

```sh
go build -buildvcs=false -trimpath -o pproxy .
systemctl --user link "$PWD/pproxy.service"
systemctl --user enable --now pproxy.service
```

To start the user service during system startup instead of waiting for login, enable lingering once:

```sh
sudo loginctl enable-linger "$USER"
```

Inspect the service with:

```sh
systemctl --user status pproxy.service
journalctl --user -u pproxy.service -f
```
