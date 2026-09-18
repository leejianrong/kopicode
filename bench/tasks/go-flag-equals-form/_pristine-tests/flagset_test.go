package flagset

import (
	"reflect"
	"testing"
)

func TestSpaceSeparatedValue(t *testing.T) {
	// --verbose comes last: a flag takes the next argument as its value when
	// that argument does not look like a flag, so a trailing operand after
	// --verbose would belong to it.
	flags, operands, err := Parse([]string{"--host", "example.com", "file.txt", "--verbose"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantFlags := map[string]string{"host": "example.com", "verbose": ""}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %v, want %v", flags, wantFlags)
	}
	if want := []string{"file.txt"}; !reflect.DeepEqual(operands, want) {
		t.Errorf("operands = %v, want %v", operands, want)
	}
}

func TestEqualsForm(t *testing.T) {
	flags, _, err := Parse([]string{"--host=example.com", "--port=8080"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{"host": "example.com", "port": "8080"}
	if !reflect.DeepEqual(flags, want) {
		t.Errorf("flags = %v, want %v", flags, want)
	}
}

func TestEqualsFormEmptyValue(t *testing.T) {
	flags, _, err := Parse([]string{"--tag=", "release.txt"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, ok := flags["tag"]; !ok || got != "" {
		t.Errorf(`flags["tag"] = %q, %v; want "", true`, got, ok)
	}
}

func TestEqualsFormEmptyName(t *testing.T) {
	if _, _, err := Parse([]string{"--=oops"}); err == nil {
		t.Error("Parse accepted a flag with an empty name")
	}
}

func TestTerminator(t *testing.T) {
	_, operands, err := Parse([]string{"--verbose", "--", "--not-a-flag"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []string{"--not-a-flag"}; !reflect.DeepEqual(operands, want) {
		t.Errorf("operands = %v, want %v", operands, want)
	}
}
