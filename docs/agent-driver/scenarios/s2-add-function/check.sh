here=$(dirname "$0")
python3 -m unittest discover -s tests >/dev/null 2>&1 || { echo "FAIL: the project's own tests do not pass"; exit 1; }
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
cp "$here/hidden/test_chunk_hidden.py" "$tmp/"
PYTHONPATH=. python3 -m unittest discover -s "$tmp" >/dev/null 2>&1 || { echo "FAIL: chunk misbehaves on the hidden cases"; exit 1; }
grep -q "chunk" tests/test_lists.py || { echo "FAIL: no tests for chunk were added to tests/test_lists.py"; exit 1; }
echo "OK"
