// Package flagset is the argument parser the deploy tool uses. It is
// hand-rolled because the tool has to pass unrecognised arguments through to
// the command it wraps.
package flagset

import (
	"fmt"
	"strings"
)

// Parse splits args into long flags and positional operands.
//
// A flag is `--name`, optionally followed by a value that does not itself look
// like a flag, or `--name=value`. A bare `--` ends flag parsing: everything
// after it is an operand, even if it starts with a dash.
func Parse(args []string) (map[string]string, []string, error) {
	flags := make(map[string]string)
	var operands []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			return flags, operands, nil
		}

		if !strings.HasPrefix(arg, "--") {
			operands = append(operands, arg)
			continue
		}

		name := strings.TrimPrefix(arg, "--")
		if name == "" {
			return nil, nil, fmt.Errorf("empty flag name in %q", arg)
		}

		if before, after, found := strings.Cut(name, "="); found {
			if before == "" {
				return nil, nil, fmt.Errorf("empty flag name in %q", arg)
			}
			flags[before] = after
			continue
		}

		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags[name] = args[i+1]
			i++
			continue
		}
		flags[name] = ""
	}

	return flags, operands, nil
}
