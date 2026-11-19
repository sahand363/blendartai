# blendmix

Headless Linux service that pairs source images, asks Google Gemini to write blended prompts, generates new images, and serves a dark-theme review gallery.

## Setup

1. Create a dedicated user and directories:
   ```bash
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin blendmix
   sudo mkdir -p /var/lib/blendmix/sources /var/lib/blendmix/out
   sudo chown -R blendmix:blendmix /var/lib/blendmix

```

2. Install the binary to `/usr/local/bin/blendmix` and create `/etc/blendmix.env` (`chmod 600`, owned by `root:blendmix`):
```env
SOURCE_DIR=/var/lib/blendmix/sources
OUT_DIR=/var/lib/blendmix/out
LISTEN_ADDR=127.0.0.1:8090
BASE_PATH=/plugins/mixer
BLENDMIX_TOKEN=replace-with-long-random-token
GEMINI_API_KEY=replace-with-gemini-api-key
TEXT_MODEL=gemini-3.1-flash-lite
IMAGE_MODEL=gemini-nano-banana-2.1
IMAGE_SIZE=1K
PRICE_PER_IMAGE=0.0336
MAX_IMAGES=100
MAX_USD=5.00
IMAGES_PER_PAIR=5

```


3. Enable and start systemd service:
```bash
sudo cp blendmix.service /etc/systemd/system/blendmix.service
sudo systemctl daemon-reload
sudo systemctl enable --now blendmix

```



## Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `SOURCE_DIR` | `./sources` | Directory containing `.png`, `.jpg`, `.jpeg`, `.webp` inputs |
| `OUT_DIR` | `./out` | Output root (`runs/` and `picked/`) |
| `LISTEN_ADDR` | `127.0.0.1:8090` | HTTP bind address |
| `BASE_PATH` | *(empty)* | Reverse-proxy path prefix (e.g. `/plugins/mixer`) |
| `BLENDMIX_TOKEN` | *(required)* | Bearer token for all `/api/*` endpoints |
| `GEMINI_API_KEY` | *(required)* | Google Gemini API key (never logged) |
| `TEXT_MODEL` | `gemini-3.1-flash-lite` | Text model used for pair description |
| `IMAGE_MODEL` | `gemini-nano-banana-2.1` | Image generation model |
| `IMAGE_SIZE` | `1K` | Requested output image size |
| `PRICE_PER_IMAGE` | `0.0336` | Estimated USD cost per generated image |
| `MAX_IMAGES` | `100` | Default max images per run |
| `MAX_USD` | `5.00` | Default max estimated USD spend per run |
| `IMAGES_PER_PAIR` | `5` | Prompts/images generated per pair |

## Reverse Proxy Examples

### Caddy

```caddyfile
example.com {
    handle /plugins/mixer* {
        reverse_proxy 127.0.0.1:8090
    }
}

```

### nginx

```nginx
location /plugins/mixer/ {
    proxy_pass [http://127.0.0.1:8090/plugins/mixer/](http://127.0.0.1:8090/plugins/mixer/);
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}

```

```

