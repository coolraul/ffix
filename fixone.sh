#!/usr/bin/env bash

set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "Usage: $0 <inputfile>"
  exit 1
fi

input="$1"

if [ ! -f "$input" ]; then
  echo "Error: file not found: $input"
  exit 1
fi

tmp="${input}.tmp"

# Run ffmpeg
ffmpeg -i "$input" -c copy -movflags +faststart "$tmp"

# Replace original on success
mv -f "$tmp" "$input"

echo "Updated: $input"
