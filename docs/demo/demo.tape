# The README demo: a failing Go test, fixed by kopicode against a live model.
# Driven by docs/demo/record.sh (run from /tmp/kd, because Output must be a relative path), which exports KD_BIN_DIR and builds /tmp/kd/ordinal first.
# Do not use Wait+Screen here: VHS read a stale screen and aborted (see docs/recording-a-demo.md).
Output take.mp4
Set Shell bash
Set Width 1280
Set Height 720
Set FontSize 17
Set TypingSpeed 30ms
Set Theme "Catppuccin Mocha"
Set Padding 24

Hide
Type "unset GOROOT; export PATH=$KD_BIN_DIR:$PATH PS1='$ ' && cd /tmp/kd/ordinal && go test ./... 2>&1 | head -4; clear"
Enter
Show

Type "go test ./..."
Sleep 500ms
Enter
Sleep 2s
Type "clear"
Enter
Sleep 400ms
Type "kopicode"
Sleep 500ms
Enter
Sleep 2500ms
Type "the tests in ordinal_test.go fail for 11, 12 and 13. fix Ordinal so they pass"
Sleep 600ms
Enter
Sleep 19s
Sleep 3s
