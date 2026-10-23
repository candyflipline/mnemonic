package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func gitRemoteID(normalizedRemote string) string {
	return "git:" + hashDomain("mnemonic-git-remote\x00", normalizedRemote)
}

func gitCommonDirID(commonDir string) string {
	return "git:" + hashDomain("moraine-git-common-dir\x00", commonDir)
}

func dirID(canonical string) string {
	return "dir:" + hashDomain("moraine-directory-root\x00", canonical)
}

func hashDomain(domain, s string) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func RemoteID(url string) (string, bool) {
	n := NormalizeRemote(url)
	if splitRemote(url).host == "" || n == "" {
		return "", false
	}
	return gitRemoteID(n), true
}

type remote struct {
	scheme, host, path string
	userinfo, scp      bool
}

func splitRemote(url string) remote {
	url = strings.TrimSpace(url)
	if scheme, rest, ok := strings.Cut(url, "://"); ok {
		authority, path, _ := strings.Cut(rest, "/")
		r := remote{scheme: scheme, path: path}
		if i := strings.LastIndex(authority, "@"); i >= 0 {
			r.userinfo, authority = true, authority[i+1:]
		}
		r.host = authority
		return r
	}
	colon := scpColon(url)
	if colon > 0 && !strings.Contains(url[:colon], "/") {
		r := remote{scp: true, host: url[:colon], path: url[colon+1:]}
		if i := strings.LastIndex(r.host, "@"); i >= 0 {
			r.userinfo, r.host = true, r.host[i+1:]
		}
		return r
	}
	return remote{path: url}
}

func scpColon(url string) int {
	off := 0
	if i := strings.Index(url, "@"); i >= 0 && !strings.Contains(url[:i], "/") {
		off = i + 1
	}
	if !strings.HasPrefix(url[off:], "[") {
		return strings.Index(url, ":")
	}
	if j := strings.Index(url[off:], "]:"); j >= 0 {
		return off + j + 1
	}
	return -1
}

func (r remote) String() string {
	switch {
	case r.scheme != "":
		return r.scheme + "://" + r.host + "/" + r.path
	case r.scp:
		return r.host + ":" + r.path
	default:
		return r.path
	}
}

func StripUserinfo(url string) string {
	r := splitRemote(url)
	if !r.userinfo {
		return strings.TrimSpace(url)
	}
	r.userinfo = false
	return r.String()
}

func NormalizeRemote(url string) string {
	r := splitRemote(url)
	host := r.host
	if !r.scp {
		if i := strings.LastIndex(host, ":"); i >= 0 && !strings.HasSuffix(host, "]") {
			host = host[:i]
		}
	}
	host = strings.ToLower(host)
	path := strings.Trim(r.path, "/")
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	switch {
	case host == "":
		return path
	case path == "":
		return host
	default:
		return host + "/" + path
	}
}
