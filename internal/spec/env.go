package spec

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Env resolves variables referenced from kindling.yaml. Process environment
// wins over the .env file, matching docker compose.
type Env struct {
	File map[string]string
}

func (e Env) Lookup(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok {
		return v, true
	}
	v, ok := e.File[key]
	return v, ok
}

// ReadDotenv parses a .env file. A missing file yields an empty map.
func ReadDotenv(path string) (map[string]string, error) {
	vars := map[string]string{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return vars, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		key, value, ok, err := parseDotenvLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if ok {
			vars[key] = value
		}
	}
	return vars, sc.Err()
}

func parseDotenvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("expected KEY=VALUE")
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	} else if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return key, value, true, nil
}

// SetDotenv sets key=value in a .env file, replacing an existing assignment or
// appending a new one, and leaves every other line untouched.
func SetDotenv(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(SetDotenvValue(string(data), key, value)), 0o600)
}

// SetDotenvValue is SetDotenv for .env content held in memory.
func SetDotenvValue(content, key, value string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}
	replaced := false
	for i, line := range lines {
		if k, _, ok, _ := parseDotenvLine(line); ok && k == key {
			lines[i] = key + "=" + value
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, key+"="+value)
	}
	return strings.Join(lines, "\n") + "\n"
}

// interpolate expands ${VAR} and ${VAR:-default}; "$$" is a literal "$".
// It reports the names of referenced variables that are unset or empty.
func interpolate(s string, env Env) (string, []string) {
	var out strings.Builder
	var missing []string
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "$$"):
			out.WriteByte('$')
			i++
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				out.WriteString(s[i:])
				return out.String(), missing
			}
			name, def, hasDef := strings.Cut(s[i+2:i+end], ":-")
			if v, ok := env.Lookup(name); ok && v != "" {
				out.WriteString(v)
			} else if hasDef {
				out.WriteString(def)
			} else {
				missing = append(missing, name)
			}
			i += end
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String(), missing
}
