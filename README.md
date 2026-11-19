# BlendArtAI

Tools that blend two images (subject + style) with Google Gemini.

- `blendmix/`: headless Go service that mixes random pairs from a source folder with Nano Banana, saves the results, and serves a review gallery. Runs behind CorText at `/plugins/mixer/`. See [blendmix/README.md](blendmix/README.md).

Releases: tag `blendmix-vX.Y.Z` to build a static linux/amd64 binary.
