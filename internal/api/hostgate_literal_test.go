package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// vmHostRe matches a SecretVM host name. Deploy files and docs must not
// carry a real one: the console host comes from WEB_PUBLIC_URL only.
var vmHostRe = regexp.MustCompile(`(?i)([a-z0-9-]+)\.vm\.scrtlabs\.com`)

// vmHostPlaceholders are the documented placeholder labels.
var vmHostPlaceholders = map[string]bool{"your-vm-name": true}

func vmHostLiterals(s string) []string {
	var out []string
	for _, m := range vmHostRe.FindAllStringSubmatch(s, -1) {
		if !vmHostPlaceholders[strings.ToLower(m[1])] {
			out = append(out, m[0])
		}
	}
	return out
}

func TestVMHostLiteralMatcher(t *testing.T) {
	if got := vmHostLiterals("rule: Host(`some-name.VM.scrtlabs.com`)"); len(got) != 1 {
		t.Fatalf("a VM host literal was not caught: %v", got)
	}
	if got := vmHostLiterals("WEB_PUBLIC_URL=https://your-vm-name.vm.scrtlabs.com and https://….vm.scrtlabs.com"); len(got) != 0 {
		t.Fatalf("placeholders flagged: %v", got)
	}
}

func TestNoVMHostLiteralInDeployOrDocs(t *testing.T) {
	root := filepath.Join("..", "..")
	targets := []string{"deploy", "docs", "README.md", "llms.txt", "llms-full.txt", "docker-compose.local.yml"}
	for _, tgt := range targets {
		base := filepath.Join(root, tgt)
		if _, err := os.Stat(base); err != nil {
			t.Fatalf("%s: %v", tgt, err)
		}
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if got := vmHostLiterals(string(b)); len(got) > 0 {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s: VM host name literal %v (use WEB_PUBLIC_URL and the your-vm-name placeholder)", rel, got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
