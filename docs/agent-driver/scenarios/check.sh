#!/bin/sh
# usage: check.sh <scenario-id> <dir>   -- exit 0 when the tree is in the known-good state, 1 otherwise.
# It looks at the tree only. What the session did on the way is for the rubric, from the journal.
set -u
here=$(cd "$(dirname "$0")" && pwd)
id=${1:?scenario id}; dir=${2:?dir}
[ -f "$here/$id/check.sh" ] || { echo "no scenario $id" >&2; exit 2; }
cd "$dir" && exec sh "$here/$id/check.sh"
