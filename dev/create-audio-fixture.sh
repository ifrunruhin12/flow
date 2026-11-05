#!/usr/bin/env sh
set -eu

flow_dev_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
flow_fixture_dir="$flow_dev_dir/music-root/library/Flow/M0"
flow_fixture="$flow_fixture_dir/01 Contract.flac"
flow_partial="$flow_fixture_dir/01 Contract.flac.part"

if [ -e "$flow_fixture" ]; then
	printf 'Fixture already exists: %s\n' "$flow_fixture"
	exit 0
fi

if ! command -v ffmpeg >/dev/null 2>&1; then
	printf 'ffmpeg is required to create the deterministic audio fixture\n' >&2
	exit 1
fi

mkdir -p "$flow_fixture_dir"
trap 'rm -f "$flow_partial"' EXIT HUP INT TERM

ffmpeg -nostdin -hide_banner -loglevel error -f lavfi \
	-i "sine=frequency=440:duration=5" \
	-map_metadata -1 -metadata artist="Flow" -metadata album="M0 Contract" \
	-metadata title="Contract" -metadata track="1" \
	-c:a flac -f flac "$flow_partial"
mv "$flow_partial" "$flow_fixture"
trap - EXIT HUP INT TERM

printf 'Created fixture: %s\n' "$flow_fixture"
