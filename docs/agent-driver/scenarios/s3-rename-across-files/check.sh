! grep -rq "fetch_user" --include='*.py' --include='*.md' . || { echo "FAIL: fetch_user still appears: $(grep -rl fetch_user --include='*.py' --include='*.md' . | tr '\n' ' ')"; exit 1; }
grep -rq "get_user" accounts/store.py || { echo "FAIL: get_user is not defined in accounts/store.py"; exit 1; }
python3 -m unittest discover -s tests >/dev/null 2>&1 || { echo "FAIL: tests do not pass"; exit 1; }
echo "OK"
