# Tidal Integration

cliamp can stream your [Tidal](https://tidal.com/) library directly through its audio pipeline. EQ, visualizer, and all effects apply. Requires a paid Tidal subscription — every paid plan includes lossless FLAC.

Tidal delivers lossless FLAC, so cliamp streams it through the same buffer-while-playing + ffmpeg pipeline used for other lossless providers. `ffmpeg` must be on `PATH`.

## Setup

The fastest path is the interactive wizard: run `cliamp setup`, pick **Tidal**, choose a stream quality, and it writes the `[tidal]` block for you.

Or configure it manually in `~/.config/cliamp/config.toml`:

```toml
[tidal]
enabled = true
quality = "lossless"
```

No developer credentials are needed — cliamp ships built-in OAuth client credentials.

Run `cliamp`, select Tidal as a provider, and press `Enter` to sign in. cliamp shows a `link.tidal.com` URL with a short device code (and opens it in your browser). Approve the device there; cliamp waits up to 5 minutes. Once authorized, credentials are cached at `~/.config/cliamp/tidal_credentials.json` and subsequent launches refresh silently.

### Quality

`quality` selects the Tidal stream tier. If omitted, cliamp uses `"lossless"`. Supported values:

| Value | Format |
|---|---|
| `"low"` | AAC 96 kbps |
| `"high"` | AAC 320 kbps |
| `"lossless"` | FLAC 16-bit / 44.1 kHz (CD) |
| `"hires"` | FLAC 24-bit up to 192 kHz (see note) |

> **Hi-Res note:** Tidal delivers `hires` as a segmented MPEG-DASH stream, which cliamp's player cannot consume yet. When a track comes back as DASH, cliamp automatically falls back to `lossless` (CD-quality FLAC) for that track. Native hi-res playback is planned.

Any other value falls back to `"lossless"`.

For hi-res-capable playback settings, see [audio-quality.md](audio-quality.md) — `bit_depth = 32` with a matching `sample_rate` is the lossless recipe.

### Custom client credentials

cliamp's built-in OAuth client credentials are shared with other open-source Tidal clients, and Tidal revokes such client IDs from time to time. If sign-in suddenly fails with a client error, you can set a fresh pair yourself without waiting for a cliamp release:

```toml
[tidal]
client_id = "your-client-id"
client_secret = "your-client-secret"
```

After changing credentials, run `cliamp tidal reset` and sign in again.

## Usage

Start directly on Tidal:

```sh
cliamp --provider tidal
```

Once authenticated, Tidal appears as a provider alongside the others. Press `T` to jump straight to Tidal, or `Esc`/`b` to open the provider browser and select it.

The provider surfaces your Tidal library:

- **Favorite Tracks**: your liked songs (up to 500).
- **Your playlists**: playlists you created or subscribed to.
- **Favorite albums**: browsable in the album view.
- **Favorite artists**: browse an artist to see their albums.

Press `Ctrl+F` while Tidal is active to search the Tidal catalog for tracks.

## Controls

When focused on the provider panel:

| Key | Action |
|---|---|
| `Up` `Down` / `j` `k` | Navigate |
| `Enter` | Load the selected playlist/album or play the selected track |
| `Ctrl+F` | Search Tidal tracks |
| `Ctrl+R` | Refresh (re-resolves stream URLs) |
| `Tab` | Switch between provider and playlist focus |
| `Esc` / `b` | Open provider browser |

After loading a playlist or album you return to the standard playlist view with all the usual controls (seek, volume, EQ, shuffle, repeat, queue, search, lyrics).

## Troubleshooting

- **Sign-in fails immediately / "client_id may be revoked"**: Tidal periodically revokes the shared client credentials cliamp ships. Set your own `client_id`/`client_secret` in the `[tidal]` section (see above), run `cliamp tidal reset`, and sign in again.
- **Device code expired**: the `link.tidal.com` code is valid for about 5 minutes. Press `Enter` on the Tidal provider again to get a fresh code.
- **Re-authenticate**: run `cliamp tidal reset` to clear stored credentials, then relaunch cliamp and select Tidal to sign in again. (Equivalent to deleting `~/.config/cliamp/tidal_credentials.json` manually.)
- **Track is unplayable / skipped**: the track may not be streamable in your region, or its stream may be encrypted for your client type. cliamp marks such tracks unplayable and moves on.
- **Hi-Res delivered as CD quality**: expected for now — see the Hi-Res note above.
- **Stalls after a long idle session**: signed stream URLs expire over time. Press `Ctrl+R` to refresh, which re-resolves the URLs.

## Requirements

- A paid Tidal subscription
- `ffmpeg` on `PATH` for FLAC/AAC decoding
- No developer/API registration: built-in credentials are used automatically

## A note on the API

cliamp uses Tidal's private client API (the same one open-source players like python-tidal-based clients use), because Tidal's official developer API only allows 30-second previews for third-party apps. cliamp streams only — it never writes decoded audio to disk.
