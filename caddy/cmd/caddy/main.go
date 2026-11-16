// Command caddy is the Caddy build used on ktx servers: the standard
// modules plus Cloudflare DNS (for the wildcard certificate's DNS challenge)
// and internal_dns (which publishes A records for internal sites).
// Versions are pinned in ../../go.mod.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddy-dns/cloudflare"
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	_ "github.com/acheong08/ktx/caddy/internaldns"
)

func main() { caddycmd.Main() }
