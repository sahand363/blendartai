# Blendartai

A self-hosted image mixer with a web panel. Drop in your images; Blendartai keeps picking two at random, has one AI model describe the **first image in the style of the second**, saves that description, has a second AI model draw it, and repeats until it has made the number of images you asked for (100 by default). Pause, resume or stop at any time to look at the results.

## Install

On an Ubuntu/Debian server (amd64 or arm64), as root:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/sahand363/blendartai/main/scripts/install.sh)
```

The installer asks for a port (default 2096) and an optional domain, then picks HTTPS on its own:

- a certificate that already exists for the domain (certbot, x-ui) is reused,
- otherwise Let's Encrypt, but only when port 80 is free or the panel is on 443 (it never takes a port from nginx, x-ui or anything else),
- otherwise a self-signed certificate.

It prints the panel address and a one-time **setup code**. Open the panel, enter the code and create your admin login.

Run the installer again at any time to update; data and settings stay.

## SSH menu

```bash
sudo blendartai
```

Start, stop, restart, login info, reset the admin password, change the port, SSL/domain, live logs, update, roll back to the previous version, uninstall.
Shortcuts: `sudo blendartai info|restart|logs|update|rollback|reset-password`.

## Using the panel

1. **Models**: add providers (OpenAI, Gemini, Anthropic, or Custom for any OpenAI-compatible API). The key is checked and the model list loaded. Mark one model **Describe** (it must see images) and one **Image** (it must make images). Anthropic can only describe. Each model has a **Test** button (a one-word chat for text models, a lookup for image-only models, so it costs almost nothing) and each provider has **Test all**, which tests the listed models one by one (use Filter to test a few). The active image model card has **Test draw**, which makes one real picture.
2. **Sources**: drag in your images (PNG, JPEG, WebP, 20 MB max each). Two or more.
3. **Dashboard**: set how many images to make and Start. Pause, Resume, Stop. Turn **Make images** off to write descriptions only (no image model needed, nothing drawn); the run then counts descriptions. The **Live** card shows the current step (Pick pair, Describe, Draw, Save) with a timer and the two source images being mixed; **Activity** is a running log; **Latest result** shows the newest description with its two sources and the image.
4. **Descriptions**: every step of a run, newest first: the content image, the style image, the description written from them, and the image drawn from it, with timings, errors, Copy and Keep. **Copy all** and **Download .txt** give every description of the run, one per paragraph. Updates live while the run is going.
5. **Gallery**: every step shows its image and description. Open one and press K to keep or X to reject.
6. **Picked**: kept images, downloadable as a zip.

**Settings** holds the describe instruction, image size (1K, 2K, 4K or WxH), an optional price per image and dollar cap, an outbound proxy for AI calls (http, https or socks5). The describe instruction defaults to a prompt that keeps the subject of the first image in the exact style of the second, and a line added to the end of every description (default: centered portrait on a pure black background) can be changed or cleared.

A run stops by itself when it reaches the target, hits the dollar cap, the provider says the quota is used up, the key is rejected, or five steps fail in a row.

## Files

- `/etc/blendartai/blendartai.env`: port, SSL, domain (edited by the menu)
- `/var/lib/blendartai/`: `settings.json` (providers and keys, mode 600), `auth.json`, `sources/`, `runs/<run>/<step>/` (image, `.json` metadata, `description.txt`, `step.json`), `runs/<run>/sources/` (copies of the sources the run used), `runs/<run>/events.log`, `picked/`

## Build

```bash
go test ./...
go build -ldflags "-X main.version=dev" ./cmd/blendartai
```

Tagging `vX.Y.Z` builds linux amd64/arm64 and publishes a release with `SHA256SUMS`.
