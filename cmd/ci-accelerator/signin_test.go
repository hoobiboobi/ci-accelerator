package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

func TestLoadSignIn(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	key := write("key", base64.StdEncoding.EncodeToString(make([]byte, 32))+"\n")
	id, secret := write("id", "cid\n"), write("secret", "gloas-x\n")
	st, err := store.Open(context.Background(), t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client := gitlab.New([]string{"https://gitlab.com"})
	good := `{"url":"https://ci.example.com","sessionKeyFile":"` + key + `","gitlab":[{"url":"https://gitlab.com","clientIdFile":"` + id + `","secretFile":"` + secret + `","admins":["id:7"]}]}`
	a, err := loadSignIn(write("good.json", good), []string{"https://gitlab.com"}, st, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ch := a.Instances(); len(ch) != 1 || ch[0].Host != "gitlab.com" || a.RedirectURL(ch[0].Instance) != "https://ci.example.com/auth/callback/"+ch[0].Instance {
		t.Errorf("instances %+v", ch)
	}
	// No URL: the console's one GitLab.
	noURL := strings.Replace(good, `"url":"https://gitlab.com",`, `"url":"",`, 1)
	if a, err := loadSignIn(write("nourl.json", noURL), []string{"https://gitlab.com"}, st, client, nil); err != nil || a.Instances()[0].Host != "gitlab.com" {
		t.Errorf("GitLab without a URL: %v", err)
	}
	if _, err := loadSignIn(write("nourl2.json", noURL), []string{"https://gitlab.com", "https://gitlab.example.com"}, st, client, nil); err == nil {
		t.Error("GitLab without a URL accepted while the console serves two")
	}
	for name, text := range map[string]string{
		"unknown field":  strings.Replace(good, `"url":"https://ci`, `"urls":"x","url":"https://ci`, 1),
		"bad key":        strings.Replace(good, key, write("short", "c2hvcnQ="), 1),
		"missing secret": strings.Replace(good, secret, filepath.Join(dir, "none"), 1),
		"other GitLab":   strings.Replace(good, `"url":"https://gitlab.com"`, `"url":"https://gitlab.example.com"`, 1),
	} {
		if _, err := loadSignIn(write(strings.ReplaceAll(name, " ", "-")+".json", text), []string{"https://gitlab.com"}, st, client, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
