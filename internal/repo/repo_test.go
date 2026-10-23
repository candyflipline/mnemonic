package repo_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/repo"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	appID    = "git:dede843175b0240dfded40c4e811b599d13b51f67c4c01a43b810329da0e443a"
	workID   = "git:3becd145d497899e9b806ee72194c040c1de48eb37c1e2d48fd9bd2258ead54a"
	dirD     = "dir:2b09f9da8f812e8c1e86b4e3d4e52fc96d2fb75c98881ee9d088c44bae01d891"
	remoteID = "git:9cb7ce896ac016f0a75bc2b6e3d19c1938c46fe5bfe64cd00010517d9c67ad74"
)

type tool struct {
	F  repo.Fields
	OK bool
}

type pick struct {
	S  repo.Seen
	OK bool
}

func TestRepo(t *testing.T) {
	book := func(js string) repo.Book {
		b, err := repo.DecodeBook([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	toolOf := func(f repo.Fields, ok bool) tool { return tool{f, ok} }
	pickOf := func(s repo.Seen, ok bool) pick { return pick{s, ok} }
	remoteOf := func(id string, ok bool) [2]any { return [2]any{id, ok} }
	decodeErr := func(js string) bool { _, err := repo.DecodeBook([]byte(js)); return err != nil }

	app := book(`[{"path":"/work/app","kind":"git","key":"/k/app/.git","from":0}]`)
	nested := book(`[{"path":"/work","kind":"git","key":"/k/work/.git","from":0},{"path":"/work/app","kind":"git","key":"/k/app/.git","from":0}]`)
	gitAndDir := book(`[{"path":"/work/app","kind":"dir","key":"/real/d","from":0},{"path":"/work/app","kind":"git","key":"/k/app/.git","from":0}]`)
	dir := book(`[{"path":"/work/d","kind":"dir","key":"/real/d","from":0}]`)
	late := book(`[{"path":"/work/app","kind":"git","key":"/k/app/.git","from":10}]`)
	withRemote := book(`[{"path":"/work/app","kind":"git","key":"/k/app/.git","remote":"https://github.com/O/r.git","from":0}]`)
	tooMany := "[" + strings.Repeat(`{"path":"/a","kind":"dir","key":"/a","from":0},`, 4096) + `{"path":"/a","kind":"dir","key":"/a","from":0}]`
	a, _ := repo.RemoteID("https://u:tok@github.com/O/r.git")
	b, _ := repo.RemoteID("git@github.com:O/r.git")
	c, _ := repo.RemoteID("ssh://git@github.com:22/O/r")

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"sibling with a shared prefix is not under the root", toolOf(app.Tool("/work", "/work/app2/x", 0)), tool{}},
		{"a path equal to the root has no rel path", toolOf(app.Tool("/work/app", "/work/app", 0)), tool{}},
		{"a joined path escaping the root via ../ is rejected", toolOf(app.Tool("/work/app", "../app2/x", 0)), tool{}},
		{"a ../ that stays inside the root resolves", toolOf(app.Tool("/work/app/sub", "../x.go", 0)), tool{repo.Fields{ProjectID: appID, RepoRelPath: "x.go", WorktreeRoot: "/work/app"}, true}},
		{"nested git root beats its parent for an event", nested.Event("/work/app/sub", 0), repo.Fields{ProjectID: appID, WorktreeRoot: "/work/app"}},
		{"nested git root beats its parent for a tool path", toolOf(nested.Tool("/work", "app/a.go", 0)), tool{repo.Fields{ProjectID: appID, RepoRelPath: "a.go", WorktreeRoot: "/work/app"}, true}},
		{"the parent root holds paths outside the nested one", toolOf(nested.Tool("/work/app", "/work/b.go", 0)), tool{repo.Fields{ProjectID: workID, RepoRelPath: "b.go", WorktreeRoot: "/work"}, true}},
		{"git beats dir", gitAndDir.Event("/work/app", 0), repo.Fields{ProjectID: appID, WorktreeRoot: "/work/app"}},
		{"dir applies when cwd is the root", dir.Event("/work/d", 0), repo.Fields{ProjectID: dirD, WorktreeRoot: "/work/d"}},
		{"dir does not apply below cwd", dir.Event("/work/d/sub", 0), repo.Fields{}},
		{"dir tool path under cwd", toolOf(dir.Tool("/work/d", "x.go", 0)), tool{repo.Fields{ProjectID: dirD, RepoRelPath: "x.go", WorktreeRoot: "/work/d"}, true}},
		{"dir tool path when cwd is not the dir", toolOf(dir.Tool("/work/d/sub", "x.go", 0)), tool{}},
		{"a fact from 10 does not reach offset 5", late.Event("/work/app", 5), repo.Fields{}},
		{"a fact from 10 reaches offset 10", late.Event("/work/app", 10), repo.Fields{ProjectID: appID, WorktreeRoot: "/work/app"}},
		{"a tool path before the fact", toolOf(late.Tool("/work/app", "x.go", 5)), tool{}},
		{"a remote keys the project", withRemote.Event("/work/app", 0), repo.Fields{ProjectID: remoteID, WorktreeRoot: "/work/app"}},
		{"empty input is the empty book", repo.Book{}.Event("/work/app", 0), repo.Fields{}},
		{"empty book digest", book("").Digest(100), ""},
		{"digest before every fact", app.Digest(0), ""},
		{"digest of one fact", app.Digest(1), "41f2e2d059c0c02972cd2758eb49828d65cdd6271dd093664fd656c4a73315ea"},
		{"a remote changes the digest", withRemote.Digest(1) != app.Digest(1), true},
		{"pick: two projects on the longest root is no answer", pickOf(repo.Pick("/w/repo/x", []repo.Seen{{Root: "/w/repo", ProjectID: "git:a"}, {Root: "/w/repo", ProjectID: "git:b"}})), pick{}},
		{"pick: nested root wins", pickOf(repo.Pick("/w/repo/x", []repo.Seen{{Root: "/w", ProjectID: "git:a"}, {Root: "/w/repo", ProjectID: "git:b"}})), pick{repo.Seen{Root: "/w/repo", ProjectID: "git:b"}, true}},
		{"pick: component boundary", pickOf(repo.Pick("/w/repo2/x", []repo.Seen{{Root: "/w", ProjectID: "git:a"}, {Root: "/w/repo", ProjectID: "git:b"}})), pick{repo.Seen{Root: "/w", ProjectID: "git:a"}, true}},
		{"pick: the same project twice is one answer", pickOf(repo.Pick("/w/repo", []repo.Seen{{Root: "/w/repo", ProjectID: "git:b"}, {Root: "/w/repo", ProjectID: "git:b"}})), pick{repo.Seen{Root: "/w/repo", ProjectID: "git:b"}, true}},
		{"pick: nothing contains the path", pickOf(repo.Pick("/x", []repo.Seen{{Root: "/w", ProjectID: "git:a"}})), pick{}},
		{"remote ids agree across spellings", []string{a, b, c}, []string{remoteID, remoteID, remoteID}},
		{"normalize remote", repo.NormalizeRemote("ssh://git@GitHub.com:22/O/r.git/"), "github.com/O/r"},
		{"a local-path origin is no identity", remoteOf(repo.RemoteID("/srv/r.git")), [2]any{"", false}},
		{"strip userinfo, https", repo.StripUserinfo("https://u:tok@github.com/O/r.git"), "https://github.com/O/r.git"},
		{"strip userinfo, scp", repo.StripUserinfo("git@github.com:O/r.git"), "github.com:O/r.git"},
		{"strip userinfo, none", repo.StripUserinfo("https://github.com/O/r.git"), "https://github.com/O/r.git"},
		{"reject: remote with userinfo", decodeErr(`[{"path":"/a","kind":"git","key":"/a/.git","remote":"https://u:tok@github.com/O/r","from":0}]`), true},
		{"reject: scp remote with a user", decodeErr(`[{"path":"/a","kind":"git","key":"/a/.git","remote":"git@github.com:O/r","from":0}]`), true},
		{"reject: relative path", decodeErr(`[{"path":"a","kind":"dir","key":"/a","from":0}]`), true},
		{"reject: path not normalized", decodeErr(`[{"path":"/a/../b","kind":"dir","key":"/b","from":0}]`), true},
		{"reject: trailing slash", decodeErr(`[{"path":"/a/","kind":"dir","key":"/a","from":0}]`), true},
		{"reject: unknown kind", decodeErr(`[{"path":"/a","kind":"svn","key":"/a","from":0}]`), true},
		{"reject: negative from", decodeErr(`[{"path":"/a","kind":"dir","key":"/a","from":-1}]`), true},
		{"reject: more than 4096 entries", decodeErr(tooMany), true},
		{"reject: not an array", decodeErr(`{"path":"/a"}`), true},
		{"reject: NUL in key", decodeErr(`[{"path":"/a","kind":"git","key":"/k\u0000https://h/r","from":0}]`), true},
		{"reject: NUL in remote", decodeErr(`[{"path":"/a","kind":"git","key":"/k","remote":"https://h/r\u0000","from":0}]`), true},
		{"reject: control character in path", decodeErr(`[{"path":"/a\nb","kind":"dir","key":"/a","from":0}]`), true},
		{"reject: empty userinfo", decodeErr(`[{"path":"/a","kind":"git","key":"/a/.git","remote":"https://@github.com/O/r","from":0}]`), true},
		{"reject: empty scp userinfo", decodeErr(`[{"path":"/a","kind":"git","key":"/a/.git","remote":"@github.com:O/r","from":0}]`), true},
		{"strip empty userinfo, https", repo.StripUserinfo("https://@github.com/O/r"), "https://github.com/O/r"},
		{"strip empty userinfo, scp", repo.StripUserinfo("@github.com:O/r"), "github.com:O/r"},
		{"ipv6 scp and ssh spellings normalize alike", []string{repo.NormalizeRemote("git@[2001:db8::1]:O/r.git"), repo.NormalizeRemote("ssh://git@[2001:db8::1]/O/r.git")}, []string{"[2001:db8::1]/O/r", "[2001:db8::1]/O/r"}},
		{"strip userinfo, ipv6 scp", repo.StripUserinfo("git@[2001:db8::1]:O/r.git"), "[2001:db8::1]:O/r.git"},
		{"accept: a valid book", decodeErr(`[{"path":"/a","kind":"git","key":"/a/.git","remote":"https://github.com/O/r","from":3}]`), false},
		{"merge keeps min from and a later remote as its own fact", repo.MergeRoots(
			[]wire.Root{{Path: "/a", Kind: wire.RootGit, Key: "/a/.git", From: 7}},
			[]wire.Root{{Path: "/a", Kind: wire.RootGit, Key: "/a/.git", From: 3}, {Path: "/a", Kind: wire.RootGit, Key: "/a/.git", Remote: "https://github.com/O/r", From: 9}}),
			[]wire.Root{{Path: "/a", Kind: wire.RootGit, Key: "/a/.git", From: 3}, {Path: "/a", Kind: wire.RootGit, Key: "/a/.git", Remote: "https://github.com/O/r", From: 9}}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}
}
