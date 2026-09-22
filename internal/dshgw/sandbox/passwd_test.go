package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPasswdPointsTheWorkerAccountAtTheGivenHome(t *testing.T) {
	host := []byte("root:x:0:0:root:/root:/bin/bash\nwinger:x:1000:1000:winger:/home/winger:/bin/bash\nother:x:1001:1001::/home/other:/bin/sh\n")
	view, err := RenderPasswd(host, "winger", "/srv/tenants/alice")
	if err != nil {
		t.Fatal(err)
	}
	want := "root:x:0:0:root:/root:/bin/bash\nwinger:x:1000:1000:winger:/srv/tenants/alice:/bin/bash\nother:x:1001:1001::/home/other:/bin/sh\n"
	if string(view) != want {
		t.Fatalf("view =\n%q\nwant\n%q", view, want)
	}
	// The renderer is pure: the caller's bytes are what it was handed.
	if !strings.Contains(string(host), "/home/winger") {
		t.Fatal("RenderPasswd rewrote its input")
	}
}

func TestRenderPasswdKeepsEveryOtherByte(t *testing.T) {
	// An NIS entry, an unrelated account, and a file with no trailing newline must all survive
	// untouched: only the worker account's home field is ours to change.
	host := []byte("+::::::\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\nwinger:x:1000:1000::/home/winger:/bin/bash")
	view, err := RenderPasswd(host, "winger", "/srv/alice")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(string(view), "\n") {
		t.Fatalf("a file without a trailing newline gained one: %q", view)
	}
	if !strings.Contains(string(view), "+::::::\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n") {
		t.Fatalf("unrelated lines changed: %q", view)
	}
	if !strings.Contains(string(view), "winger:x:1000:1000::/srv/alice:/bin/bash") {
		t.Fatalf("the worker home was not rewritten: %q", view)
	}
}

func TestRenderPasswdRefusesUnusableInput(t *testing.T) {
	host := []byte("winger:x:1000:1000::/home/winger:/bin/bash\n")
	for _, tc := range []struct{ name, user, home string }{
		{"an account that is not in the file", "dshgw", "/srv/alice"},
		{"a blank account", " ", "/srv/alice"},
		{"a relative home", "winger", "srv/alice"},
		{"a home with a field separator", "winger", "/srv/a:b"},
		{"a home with a newline", "winger", "/srv/a\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderPasswd(host, tc.user, tc.home); err == nil {
				t.Fatalf("RenderPasswd(%q, %q) was accepted", tc.user, tc.home)
			}
		})
	}
}

func TestProfileBindsTheRenderedPasswdViewWhenGiven(t *testing.T) {
	f := newProfileFixture(t)
	f.alice.PasswdFile = filepath.Join(f.root, "state/tenants/alice/.dsh/sandbox/passwd")
	argv, err := Profile(f.rt, f.alice)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ := parseMounts(t, argv)
	found := false
	for _, m := range mounts {
		if m.dst != "/etc/passwd" {
			continue
		}
		found = true
		if m.src != f.alice.PasswdFile {
			t.Fatalf("/etc/passwd is bound from %q, want the tenant view %q", m.src, f.alice.PasswdFile)
		}
		// A view is written moments before the exec, so a missing one is a fault rather than a
		// deployment that simply has no such file.
		if m.flag != "--ro-bind" {
			t.Fatalf("the passwd view is bound with %s, want --ro-bind", m.flag)
		}
	}
	if !found {
		t.Fatalf("profile mounts no /etc/passwd at all:\n%v", argv)
	}

	// Without a view the host file is still tolerated-missing, as it always was.
	plain := f.alice
	plain.PasswdFile = ""
	argv, err = Profile(f.rt, plain)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _, _ = parseMounts(t, argv)
	for _, m := range mounts {
		if m.dst == "/etc/passwd" && (m.flag != "--ro-bind-try" || m.src != "/etc/passwd") {
			t.Fatalf("the host passwd is bound as %s %s, want --ro-bind-try /etc/passwd", m.flag, m.src)
		}
	}

	// A view that is not an absolute path is refused before bubblewrap ever sees it.
	f.alice.PasswdFile = "sandbox/passwd"
	if _, err := Profile(f.rt, f.alice); err == nil {
		t.Fatal("a relative passwd view produced a profile")
	}
}
