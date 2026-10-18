python3 -m unittest discover -s tests >/dev/null 2>&1 || { echo "FAIL: tests do not pass"; exit 1; }
git diff --quiet HEAD -- tests || { echo "FAIL: tests were changed"; exit 1; }
changed=$(git status --porcelain | grep -v '^?? ' | awk '{print $2}' | grep -v '^shop/pagination.py$')
[ -z "$changed" ] || { echo "FAIL: files outside shop/pagination.py changed: $changed"; exit 1; }
echo "OK"
