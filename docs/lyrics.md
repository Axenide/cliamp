# Lyrics

Press `y` to show lyrics for the current track. For a local file, cliamp first uses lyrics embedded in file tags. If the file has no embedded lyrics, cliamp fetches lyrics from LRCLIB and NetEase Cloud Music.

## Modes

- **Synced lyrics**: For local files, Navidrome tracks, and YouTube/yt-dlp tracks with a known duration, lyrics scroll automatically and highlight the active line during playback.
- **Scroll mode**: For plain lyrics without timestamps, live radio (ICY), and YouTube Live, use `j`/`k` or the arrow keys to scroll manually. The YouTube Live position is not relative to the song.

cliamp keeps timestamps in embedded LRC lyrics. It shows embedded plain-text lyrics in scroll mode.

## Streams

cliamp updates lyrics when ICY metadata changes, for example when an internet radio station changes tracks.

## YouTube and SoundCloud

cliamp parses titles such as "Artist - Song (Official Video)" to build search queries.
