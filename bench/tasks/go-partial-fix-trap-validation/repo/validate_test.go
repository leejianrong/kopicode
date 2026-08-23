package profile

import "testing"

func TestAcceptsValidNames(t *testing.T) {
	names := []string{
		"Mary-Jane",
		"O'Brien",
		"Anna Maria",
		"Jose",
		"Zoe",
		"D'Angelo Smith",
	}
	for _, name := range names {
		if err := ValidateDisplayName(name); err != nil {
			t.Errorf("ValidateDisplayName(%q) = %v, want nil", name, err)
		}
	}
}

func TestRejectsTooLongName(t *testing.T) {
	name := "This Display Name Is Deliberately Way Too Long"
	if len(name) <= maxDisplayNameLength {
		t.Fatalf("test fixture is %d characters, want more than %d", len(name), maxDisplayNameLength)
	}
	if err := ValidateDisplayName(name); err == nil {
		t.Errorf("ValidateDisplayName(%q) = nil, want an error", name)
	}
}

func TestRejectsDisallowedCharacter(t *testing.T) {
	names := []string{
		"Anna@Home",
		"Bob#1",
		"<script>",
	}
	for _, name := range names {
		if err := ValidateDisplayName(name); err == nil {
			t.Errorf("ValidateDisplayName(%q) = nil, want an error", name)
		}
	}
}

// TestRejectsEmptyName is the exact symptom finance reported: a profile saved
// with the display name field submitted as a plain empty string.
func TestRejectsEmptyName(t *testing.T) {
	if err := ValidateDisplayName(""); err == nil {
		t.Errorf("ValidateDisplayName(\"\") = nil, want an error rejecting a blank display name")
	}
}

// TestRejectsWhitespaceOnlyName covers the closely related case nobody filed a
// ticket about: a display name made up entirely of spaces or other
// whitespace is just as blank as "" once it is rendered, and the allowed
// character set already lets space through, so this slips past a fix that
// only special-cases the exact empty string.
func TestRejectsWhitespaceOnlyName(t *testing.T) {
	names := []string{
		"   ",
		"\t",
		" \t \n ",
		"\n\n",
	}
	for _, name := range names {
		if err := ValidateDisplayName(name); err == nil {
			t.Errorf("ValidateDisplayName(%q) = nil, want an error rejecting a blank display name", name)
		}
	}
}
