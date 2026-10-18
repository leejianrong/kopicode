#!/bin/sh
# usage: setup.sh <scenario-id> <empty-dir>
# Copies a scenario's fixture into <empty-dir>, makes it a fresh git repository with one commit, and
# tells kopicode how to verify it. A throwaway repo (not a worktree), so it shares nothing with any checkout.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
id=${1:?scenario id}; dir=${2:?target dir}
[ -d "$here/$id/fixture" ] || { echo "no scenario $id" >&2; exit 2; }
[ -z "$(ls -A "$dir" 2>/dev/null)" ] || { echo "$dir is not empty" >&2; exit 2; }
mkdir -p "$dir"
cp -R "$here/$id/fixture/." "$dir/"
mkdir -p "$dir/.kopicode"
printf 'verify = ["python3", "-m", "unittest", "discover", "-s", "tests"]\n' > "$dir/.kopicode/config.toml"
cd "$dir"
git init -q .
printf '.kopicode/\n__pycache__/\n' > .git/info/exclude
git add -A
git -c user.name=scenario -c user.email=scenario@example.invalid commit -qm "scenario $id: start"
echo "ready: $dir"
