#!/usr/bin/env sh
set -eu

flow_m3_dev_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
flow_m3_album="$flow_m3_dev_dir/music-root/library/Flow/M3"
mkdir -p "$flow_m3_album"
command -v ffmpeg >/dev/null 2>&1 || { printf 'ffmpeg is required\n' >&2; exit 1; }

create_track() {
    flow_m3_track="$1"
    flow_m3_title="$2"
    flow_m3_duration="$3"
    flow_m3_destination="$flow_m3_album/$flow_m3_track $flow_m3_title.flac"
    [ ! -e "$flow_m3_destination" ] || return 0
    flow_m3_partial=$(mktemp "$flow_m3_album/.flow-m3-fixture.XXXXXX")
    trap 'rm -f "$flow_m3_partial"' EXIT HUP INT TERM
    ffmpeg -y -nostdin -hide_banner -loglevel error -f lavfi \
        -i "sine=frequency=440:duration=$flow_m3_duration" \
        -map_metadata -1 -metadata artist="Flow" -metadata album="M3 Playback" \
        -metadata title="$flow_m3_title" -metadata track="$flow_m3_track" \
        -c:a flac -f flac "$flow_m3_partial"
    mv -n "$flow_m3_partial" "$flow_m3_destination"
    rm -f "$flow_m3_partial"
    trap - EXIT HUP INT TERM
}

# Longer than the 10-second checkpoint interval, plus a short transition track.
create_track 01 Checkpoint 30
create_track 02 Transition 5
printf 'M3 playback fixtures are ready: %s\n' "$flow_m3_album"
