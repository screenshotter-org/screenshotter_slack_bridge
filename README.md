# Screenshotter Slack bridge

Renders Screenshotter links posted in Slack as inline image previews. It
connects to Slack over Socket Mode — an outbound WebSocket — and fetches
screenshots over your own network, so the Screenshotter server needs no public
exposure and no inbound path from Slack. See also the
[Screenshotter server](https://github.com/screenshotter-org/screenshotter_server) it pairs
with.

A pre-built container image is published at
[ghcr.io/screenshotter-org/screenshotter-slack-bridge](https://github.com/orgs/screenshotter-org/packages/container/package/screenshotter-slack-bridge).

Full documentation — Slack app setup, every configuration option, and
deployment — is at
[www.screenshotter.org/slack.html](https://www.screenshotter.org/slack.html).

## How it works

When someone posts a link on one of the domains you register, Slack delivers a
`link_shared` event over the WebSocket. The bridge fetches the PNG from the
Screenshotter server, registers it with Slack as a *remote file* whose preview
Slack stores workspace-privately, and replaces the link with a card showing that
preview. The card links to the screenshot's page on your server, so the
full-size image never leaves your network.

## Container

```sh
cp config.example.toml config.toml
$EDITOR config.toml

docker run --rm \
  -v "$PWD/config.toml:/app/config.toml:ro" \
  -e SLACK_BOT_TOKEN -e SLACK_APP_TOKEN \
  ghcr.io/screenshotter-org/screenshotter-slack-bridge:latest
```

The container needs only the config file and the two token variables. Its egress
must reach `*.slack.com` and the Screenshotter server.

## Install with Go

```sh
go install github.com/screenshotter-org/screenshotter_slack_bridge/cmd/screenshotter-slack-bridge@latest
```

No C compiler needed. Versions before `v0.2.0` cannot be installed this way:
their `go.mod` carried a `replace` directive, which `go install` refuses.

## Build from source

Go 1.26 or later; no CGo.

```sh
go build -o screenshotter-slack-bridge ./cmd/screenshotter-slack-bridge
./screenshotter-slack-bridge -config config.toml
```

`just build`, `just test` and `just lint` wrap the usual commands if you have
[just](https://github.com/casey/just) installed.

## Configuration

Copy [`config.example.toml`](config.example.toml) to `config.toml` and edit it;
every option is documented inline there. The three required settings are:

- `screenshotter_base_url` — the canonical base URL of your Screenshotter
  server. The bridge fetches `<base_url>/<id>.png` from it and uses
  `<base_url>/<id>` as the card's click-through. It may be a private address.
  Where the bridge reaches the server on a different address than the readers
  do — a Kubernetes Service, say — the optional `screenshotter_fetch_base_url`
  overrides it for the bridge's own fetches.
- `unfurl_domains` — the hostnames to unfurl, matching the "App unfurl domains"
  registered in your Slack app.
- The two Slack tokens, a bot token (`xoxb-…`) and an app-level token
  (`xapp-…`). Prefer the `bot_token_env_var` / `app_token_env_var` forms so the
  secrets stay out of the config file.

The Slack app needs the `links:read`, `links:write`, `remote_files:write` and
`remote_files:read` bot scopes, plus a `connections:write` app-level token for
Socket Mode. The bridge assumes the server runs with
`require_auth_to_view = false`: the image ID is the capability.

### Card styles

`card_style` picks how the card looks. The default, `"image"`, shows the whole
preview under its title, followed by a footer line — an icon, a
`card_caption` link ("Open in Screenshotter") to the screenshot's page and a
`card_source_caption` link ("Open source page") to the page it was taken from;
`""` removes either link, and the footer goes when both are empty. Slack
fetches that icon, `card_favicon_url`, itself, so it must be a public URL; it
defaults to the icon on www.screenshotter.org, and `""` removes it. `"file"` shows
Slack's own file card instead, which crops the preview and puts a file header
above it, but has no coloured bar down its left edge.

## Slack app setup

See [www.screenshotter.org/slack/configuration.html](https://www.screenshotter.org/slack/configuration.html)
for the step-by-step walkthrough of creating the app, collecting its two tokens
and registering the unfurl domains.
