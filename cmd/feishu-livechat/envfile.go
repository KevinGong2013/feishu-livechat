package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultEnvFile is where init writes the configuration and where serve,
// setup and token look for it. Real environment variables win over it.
const defaultEnvFile = "feishu-livechat.env"

type envVar struct {
	key, value string
}

// loadEnvFile sets the KEY=VALUE pairs in path that are not already in the
// environment. A missing file is not an error.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

// writeEnvFile writes the configuration readable by the owner only: it
// holds the app secret.
func writeEnvFile(path string, vars []envVar) error {
	var b strings.Builder
	b.WriteString("# feishu-livechat configuration, written by `feishu-livechat init`.\n")
	b.WriteString("# Keep it secret: it holds the app secret and the token signing key.\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "%s=%s\n", v.key, v.value)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".feishu-livechat-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
