#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PIE_RELAY_PROFILE=kroot-studio exec "$script_dir/provision-profile.sh"
