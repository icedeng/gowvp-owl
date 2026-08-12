package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureZLMSecretFormats(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "api section", content: "[api]\nsecret=old\n[http]\nport=80\n", want: "[api]\nsecret=new-secret\n[http]"},
		{name: "prefixed", content: "api.secret=old\nhttp.port=80\n", want: "api.secret=new-secret\nhttp.port=80"},
		{name: "unrelated section secret", content: "[other]\nsecret=keep\n", want: "[other]\nsecret=keep\n\n[api]\nsecret=new-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "zlm.ini")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := ensureZLMSecret(path, "new-secret"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Fatalf("config = %q, want substring %q", got, tc.want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestEnsureZLMSecretRejectsEmpty(t *testing.T) {
	if err := ensureZLMSecret(filepath.Join(t.TempDir(), "zlm.ini"), " "); err == nil {
		t.Fatal("expected empty secret to be rejected")
	}
}
