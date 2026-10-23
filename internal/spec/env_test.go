package spec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDotenv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := `# comment
PLAIN=value
export EXPORTED=yes
QUOTED="has # hash"
SINGLE='x y'
TRAILING=abc # note
EMPTY=
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDotenv(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PLAIN": "value", "EXPORTED": "yes", "QUOTED": "has # hash",
		"SINGLE": "x y", "TRAILING": "abc", "EMPTY": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	if vars, err := ReadDotenv(filepath.Join(t.TempDir(), "missing")); err != nil || len(vars) != 0 {
		t.Errorf("missing file: %v, %v", vars, err)
	}
}

func TestSetDotenvValue(t *testing.T) {
	in := "# keep me\nA=1\nB=2\n"
	if got, want := SetDotenvValue(in, "A", "9"), "# keep me\nA=9\nB=2\n"; got != want {
		t.Errorf("replace: got %q, want %q", got, want)
	}
	if got, want := SetDotenvValue(in, "C", "3"), "# keep me\nA=1\nB=2\nC=3\n"; got != want {
		t.Errorf("append: got %q, want %q", got, want)
	}
	if got, want := SetDotenvValue("", "A", "1"), "A=1\n"; got != want {
		t.Errorf("empty: got %q, want %q", got, want)
	}
}

func TestEnvPrefersProcessEnvironment(t *testing.T) {
	t.Setenv("KINDLING_TEST_VAR", "from-process")
	env := Env{File: map[string]string{"KINDLING_TEST_VAR": "from-file"}}
	if v, _ := env.Lookup("KINDLING_TEST_VAR"); v != "from-process" {
		t.Errorf("Lookup = %q, want process value", v)
	}
}
